package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

const categoryIndexVersion = 1

var (
	// ErrInvalidCategory reports a category name that cannot be used as a
	// persistent identifier or a single directory component.
	ErrInvalidCategory = errors.New("session: invalid category")
	// ErrCategoryExists reports an attempt to create a duplicate category.
	ErrCategoryExists = errors.New("session: category already exists")
	// ErrUnknownCategory reports an assignment to a category that is not saved.
	ErrUnknownCategory = errors.New("session: unknown category")
	// ErrCategoryNamespaceConflict reports a category name that would shadow an
	// existing unclassified mount entry.
	ErrCategoryNamespaceConflict = errors.New("session: category namespace conflict")
)

type categoryIndex struct {
	Version    int            `json:"version"`
	Categories []CategoryView `json:"categories"`
}

// CategoryView is the durable category shown by the management API.
type CategoryView struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

func validateCategoryName(name string) error {
	if name == "" || strings.TrimSpace(name) != name || !utf8.ValidString(name) || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") || len([]byte(name)) > 255 {
		return fmt.Errorf("%w: category name is not a valid directory component", ErrInvalidCategory)
	}
	return nil
}

func (s *Session) categoryIndexPath() string {
	return s.categoriesPath
}

func (s *Session) categoryNamesLocked() []string {
	names := make([]string, 0, len(s.categories))
	for name := range s.categories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *Session) categoryIndexLocked() categoryIndex {
	categories := make([]CategoryView, 0, len(s.categories))
	for _, category := range s.categories {
		categories = append(categories, category)
	}
	sort.Slice(categories, func(i, j int) bool { return categories[i].Name < categories[j].Name })
	return categoryIndex{Version: categoryIndexVersion, Categories: categories}
}

func (s *Session) writeCategoriesLocked() error {
	data, err := json.Marshal(s.categoryIndexLocked())
	if err != nil {
		return fmt.Errorf("session: encode categories: %w", err)
	}
	if err := writeFileAtomic(s.categoryIndexPath(), data); err != nil {
		return fmt.Errorf("session: write categories: %w", err)
	}
	return nil
}

// loadCategories restores the category index before registry entries are
// validated, so every persisted torrent reference can be checked strictly.
func (s *Session) loadCategories() error {
	path := s.categoryIndexPath()
	if _, err := requireRegularFile(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("session: inspect categories: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("session: read categories: %w", err)
	}
	var index categoryIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return fmt.Errorf("session: decode categories: %w", err)
	}
	if index.Version != categoryIndexVersion {
		return fmt.Errorf("session: unsupported categories version %d", index.Version)
	}
	categories := make(map[string]CategoryView, len(index.Categories))
	for _, category := range index.Categories {
		if err := validateCategoryName(category.Name); err != nil {
			return fmt.Errorf("session: category %q: %w", category.Name, err)
		}
		if category.CreatedAt.IsZero() {
			return fmt.Errorf("session: category %q has no created_at", category.Name)
		}
		if _, exists := categories[category.Name]; exists {
			return fmt.Errorf("session: duplicate category %q", category.Name)
		}
		categories[category.Name] = category
	}
	s.mu.Lock()
	s.categories = categories
	s.mu.Unlock()
	return nil
}

// ListCategories returns all saved categories in stable name order.
func (s *Session) ListCategories() []CategoryView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state != stateActive {
		return []CategoryView{}
	}
	index := s.categoryIndexLocked()
	return index.Categories
}

// Categories returns category directory names for the read-only filesystem.
func (s *Session) Categories() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state != stateActive {
		return nil
	}
	return s.categoryNamesLocked()
}

// CreateCategory creates and durably records one category. The category name
// becomes a stable top-level mount directory and is not renamed or deleted.
func (s *Session) CreateCategory(ctx context.Context, name string) (CategoryView, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return CategoryView{}, err
	}
	if err := validateCategoryName(name); err != nil {
		return CategoryView{}, err
	}

	s.rootNamespaceMu.Lock()
	defer s.rootNamespaceMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureActiveLocked(); err != nil {
		return CategoryView{}, err
	}
	if _, exists := s.categories[name]; exists {
		return CategoryView{}, ErrCategoryExists
	}
	if filesystem.CategoryNameConflicts(name, s.filesystemViewsLocked(), s.categoryNamesLocked()) {
		return CategoryView{}, ErrCategoryNamespaceConflict
	}
	category := CategoryView{Name: name, CreatedAt: time.Now().UTC()}
	s.categories[name] = category
	if err := s.writeCategoriesLocked(); err != nil {
		delete(s.categories, name)
		return CategoryView{}, err
	}
	return category, nil
}

// SetTorrentCategory changes one torrent's category and persists the sidecar
// before publishing the new view. An empty category removes the association.
func (s *Session) SetTorrentCategory(ctx context.Context, id, category string) (TorrentView, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return TorrentView{}, err
	}
	hash, err := parseInfoHash(id)
	if err != nil {
		return TorrentView{}, ErrUnknownTorrent
	}
	if category != "" {
		if err := validateCategoryName(category); err != nil {
			return TorrentView{}, err
		}
	}

	s.rootNamespaceMu.Lock()
	defer s.rootNamespaceMu.Unlock()
	unlock := s.lockHash(hash)
	defer unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureActiveLocked(); err != nil {
		return TorrentView{}, err
	}
	entry, ok := s.states[hash]
	if !ok {
		return TorrentView{}, ErrUnknownTorrent
	}
	if entry.State == StateDeleting || entry.State == StateDeleteFailed {
		return TorrentView{}, ErrDeleting
	}
	if category != "" {
		if _, ok := s.categories[category]; !ok {
			return TorrentView{}, ErrUnknownCategory
		}
	}
	if entry.Category == category {
		return s.buildView(hash, s.torrents[hash], entry), nil
	}
	if err := s.subtitleCategoryConflictLocked(hash, category); err != nil {
		return TorrentView{}, err
	}
	updated := cloneRegistryEntry(entry)
	updated.Category = category
	if err := s.writeRegistryEntryLocked(updated); err != nil {
		return TorrentView{}, err
	}
	s.states[hash] = updated
	return s.buildView(hash, s.torrents[hash], updated), nil
}

func (s *Session) subtitleCategoryConflictLocked(hash metainfo.Hash, category string) error {
	views := s.filesystemViewsLocked()
	for i := range views {
		if views[i].Hash == hash {
			views[i].Category = category
			break
		}
	}
	return s.subtitleLayoutConflictLocked(views)
}
