package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"golang.org/x/sys/unix"
)

const layoutVersion = "2"

// TorrentState is the lifecycle state exposed by the management API.
type TorrentState string

const (
	StateAdding TorrentState = "adding"
	// StateReady means the metainfo is available and the torrent can be read
	// and seeded from whatever the cache currently holds. There is no
	// downloading/seeding distinction: neither is a property of a memory-only
	// piece cache.
	StateReady        TorrentState = "ready"
	StateError        TorrentState = "error"
	StateDeleting     TorrentState = "deleting"
	StateDeleteFailed TorrentState = "delete_failed"
	// StateDeleted is an in-memory terminal state of a deletion operation. It
	// is never written to a sidecar: a completed deletion has no sidecar left.
	StateDeleted TorrentState = "deleted"
)

// TorrentView is the management API's per-torrent snapshot.
type TorrentView struct {
	ID         string
	InfoHash   string
	Name       string
	State      TorrentState
	TotalBytes int64
	// DownloadedBytes is useful torrent payload read since this torrent's
	// runtime handle was registered in the current Session. It is not persisted.
	DownloadedBytes int64
	// UploadedBytes is torrent data payload sent since this torrent's runtime
	// handle was registered in the current Session. It is not persisted.
	UploadedBytes int64
	// CachedBytes is how many of the torrent's bytes are resident in the piece
	// cache right now. It falls as pieces are evicted; it is not a download
	// counter.
	CachedBytes int64
	CreatedAt   time.Time
	// Favorite marks a torrent the user wants to keep. It is persisted in the
	// sidecar and only exempts the torrent from age-based pruning; a direct
	// delete still removes it.
	Favorite bool
	Error    string
}

// Operation tracks one deletion request. It is returned by DeleteTorrent and
// polled through Operation.
type Operation struct {
	ID        string
	TorrentID string
	State     TorrentState
	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (o *Operation) clone() Operation {
	return *o
}

// registryEntry is the durable per-torrent sidecar written to
// <torrents-dir>/.metadata/state/<info_hash>.json.
type registryEntry struct {
	ID          string       `json:"id"`
	InfoHash    string       `json:"info_hash"`
	Name        string       `json:"name"`
	State       TorrentState `json:"state"`
	Error       string       `json:"error,omitempty"`
	OperationID string       `json:"operation_id,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	// Favorite is a plain bool so sidecars written before the field existed
	// decode to false instead of failing validation.
	Favorite bool `json:"favorite,omitempty"`
}

func cloneRegistryEntry(entry *registryEntry) *registryEntry {
	if entry == nil {
		return nil
	}
	clone := *entry
	return &clone
}

func (s *Session) registryPath(hash metainfo.Hash) string {
	return filepath.Join(s.stateDir, hash.HexString()+".json")
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".torrentfs-state-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync directory %q: %w", dir, err)
	}
	return nil
}

func renameNoReplace(oldPath, newPath string) error {
	err := unix.Renameat2(unix.AT_FDCWD, oldPath, unix.AT_FDCWD, newPath, unix.RENAME_NOREPLACE)
	if err == nil {
		if err := syncDirectory(filepath.Dir(newPath)); err != nil {
			return err
		}
		if filepath.Dir(oldPath) != filepath.Dir(newPath) {
			if err := syncDirectory(filepath.Dir(oldPath)); err != nil {
				return err
			}
		}
		return nil
	}
	if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EINVAL) {
		return err
	}
	// Linux supplies renameat2 in supported deployments. The link fallback
	// keeps the no-clobber property on filesystems or test kernels without it;
	// a retry after a crash observes both names and completes the move.
	if err := os.Link(oldPath, newPath); err != nil {
		return err
	}
	if err := os.Remove(oldPath); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(newPath)); err != nil {
		return err
	}
	if filepath.Dir(oldPath) != filepath.Dir(newPath) {
		if err := syncDirectory(filepath.Dir(oldPath)); err != nil {
			return err
		}
	}
	return nil
}

// publishNoClobber atomically publishes a new file without replacing an
// existing path. The returned bool reports whether this call created it.
func publishNoClobber(path string, data []byte) (bool, error) {
	dir := filepath.Dir(path)
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return false, fmt.Errorf("path %q is not a regular file", path)
		}
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	f, err := os.CreateTemp(dir, ".torrentfs-publish-*.tmp")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return false, err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return false, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if err := renameNoReplace(tmp, path); err != nil {
		_ = os.Remove(tmp)
		if errors.Is(err, unix.EEXIST) || errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func requireRegularFile(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("path %q must be a regular, non-symlink file", path)
	}
	return info, nil
}

func removeRegularFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("path %q must be a regular, non-symlink file", path)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

// writeRegistryEntryLocked durably records one torrent's state. It must be
// called with s.mu held so a deletion marker is on disk before any cleanup.
func (s *Session) writeRegistryEntryLocked(entry *registryEntry) error {
	hash, err := parseInfoHash(entry.InfoHash)
	if err != nil {
		return err
	}
	if entry.ID != hash.HexString() || entry.InfoHash != hash.HexString() {
		return fmt.Errorf("session: state identity does not match %s", hash)
	}
	if !validTorrentState(entry.State) || entry.State == StateDeleted {
		return fmt.Errorf("session: invalid state %q for %s", entry.State, hash)
	}
	if entry.CreatedAt.IsZero() {
		return fmt.Errorf("session: state %s has no created_at", hash)
	}
	entry.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("session: encode state %s: %w", entry.InfoHash, err)
	}
	if err := writeFileAtomic(s.registryPath(hash), data); err != nil {
		return fmt.Errorf("session: write state %s: %w", entry.InfoHash, err)
	}
	return nil
}

func (s *Session) removeRegistryEntryLocked(hash metainfo.Hash) error {
	path := s.registryPath(hash)
	if _, err := requireRegularFile(path); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("session: remove state %s: %w", hash, err)
		}
	} else {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("session: remove state %s: %w", hash, err)
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return fmt.Errorf("session: sync state directory: %w", err)
		}
	}
	delete(s.states, hash)
	return nil
}

func validTorrentState(state TorrentState) bool {
	switch state {
	case StateAdding, StateReady, StateError, StateDeleting, StateDeleteFailed:
		return true
	default:
		return false
	}
}

func validateRegistryEntry(name string, entry *registryEntry) (metainfo.Hash, error) {
	if entry.ID == "" || entry.InfoHash == "" {
		return metainfo.Hash{}, errors.New("missing id or info_hash")
	}
	hash, err := parseInfoHash(entry.InfoHash)
	if err != nil {
		return metainfo.Hash{}, err
	}
	canonical := hash.HexString()
	if name != canonical+".json" {
		return metainfo.Hash{}, fmt.Errorf("filename %q does not match info hash %s", name, canonical)
	}
	if entry.ID != canonical || entry.InfoHash != canonical {
		return metainfo.Hash{}, fmt.Errorf("id/info_hash do not match %s", canonical)
	}
	if !validTorrentState(entry.State) || entry.State == StateDeleted {
		return metainfo.Hash{}, fmt.Errorf("invalid state %q", entry.State)
	}
	if entry.CreatedAt.IsZero() || entry.UpdatedAt.IsZero() {
		return metainfo.Hash{}, errors.New("created_at and updated_at are required")
	}
	return hash, nil
}

// loadRegistry restores durable per-torrent state from the state directory.
func (s *Session) loadRegistry() error {
	entries, err := os.ReadDir(s.stateDir)
	if err != nil {
		return fmt.Errorf("session: scan state dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range names {
		path := filepath.Join(s.stateDir, name)
		if _, err := requireRegularFile(path); err != nil {
			return fmt.Errorf("session: inspect state %q: %w", name, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("session: read state %q: %w", name, err)
		}
		var entry registryEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			return fmt.Errorf("session: decode state %q: %w", name, err)
		}
		hash, err := validateRegistryEntry(name, &entry)
		if err != nil {
			return fmt.Errorf("session: state %q: %w", name, err)
		}
		if entry.State == StateDeleting && entry.OperationID == "" {
			opID, err := newOperationID()
			if err != nil {
				return fmt.Errorf("session: generate delete operation for %s: %w", hash, err)
			}
			entry.OperationID = opID
			if err := s.writeRegistryEntryLocked(&entry); err != nil {
				return err
			}
		}
		s.states[hash] = &entry
	}
	return nil
}

func newOperationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// parseInfoHash parses a canonical 40-character hex info hash.
func parseInfoHash(raw string) (metainfo.Hash, error) {
	if len(raw) != 40 {
		return metainfo.Hash{}, fmt.Errorf("session: invalid info hash %q", raw)
	}
	b, err := hex.DecodeString(strings.ToLower(raw))
	if err != nil || len(b) != metainfo.HashSize {
		return metainfo.Hash{}, fmt.Errorf("session: invalid info hash %q", raw)
	}
	var hash metainfo.Hash
	copy(hash[:], b)
	return hash, nil
}
