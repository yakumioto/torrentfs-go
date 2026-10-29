package vfs

import (
	"context"
	"io"
	"io/fs"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

// Entry is one observation of a virtual object. Path is absolute inside this
// tree, not a host path. Identity survives changes to the display/root name.
type Entry struct {
	Name, Path            string
	IsDir                 bool
	Size                  int64
	CreatedAt, ModifiedAt time.Time
	Identity              uint64
	ref                   reference
}

type reference struct {
	hash     metainfo.Hash
	rel      string
	category string
	kind     entryKind
}

type entryKind uint8

const (
	rootKind entryKind = iota
	categoryKind
	directoryKind
	payloadKind
	subtitleKind
)

type directoryVersion struct {
	filesID   *FileListID
	files     []FileView
	subtitles []SubtitleView
	prefixes  map[string]*directoryIndex
}

type directoryIndex struct {
	entries  []fsEntry
	byName   map[string]fsEntry
	owner    *directoryVersion
	snapshot *Directory // guarded by Tree.directories.mu
}

type Tree struct {
	backend     Backend
	mu          sync.Mutex
	nextID      uint64
	nextVersion uint64
	ids         map[string]uint64
	directories snapshotCache[directoryVersion]
}

func New(backend Backend) *Tree {
	return &Tree{backend: backend, nextID: 1, ids: map[string]uint64{"/": 1}}
}

// Backend exposes the existing source contract to the FUSE adapter. Namespace
// rules and projections remain in this package; no protocol appears in Backend.
func (t *Tree) Backend() Backend { return t.backend }

func (t *Tree) identity(key string) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id, ok := t.ids[key]; ok {
		return id
	}
	t.nextID++
	t.ids[key] = t.nextID
	return t.nextID
}

func virtualParts(raw string) ([]string, error) {
	if strings.ContainsRune(raw, 0) {
		return nil, ErrInvalidName
	}
	var out []string
	for _, part := range strings.Split(raw, "/") {
		switch part {
		case "":
			continue
		case ".", "..":
			return nil, ErrInvalidName
		default:
			out = append(out, part)
		}
	}
	return out, nil
}

func (t *Tree) Lookup(raw string) (Entry, error) {
	parts, err := virtualParts(raw)
	if err != nil {
		return Entry{}, err
	}
	entry := Entry{Name: "/", Path: "/", IsDir: true, Identity: 1}
	for _, part := range parts {
		if entry.ref.kind == directoryKind {
			contents, index, err := t.directory(entry.ref.hash, entry.ref.rel)
			if err != nil {
				return Entry{}, err
			}
			child, ok := index.byName[part]
			if !ok {
				return Entry{}, ErrNotFound
			}
			entry = t.layoutChild(entry, child, contents)
			continue
		}
		children, err := t.children(entry)
		if err != nil {
			return Entry{}, err
		}
		found := false
		for _, child := range children {
			if child.Name == part {
				entry, found = child, true
				break
			}
		}
		if !found {
			return Entry{}, ErrNotFound
		}
	}
	return entry, nil
}

// Stat refreshes generated-file and managed-subtitle metadata without opening
// payload bytes. An opened handle instead owns its immutable metadata snapshot.
func (t *Tree) Stat(raw string) (Entry, error) {
	entry, err := t.Lookup(raw)
	if err != nil {
		return Entry{}, err
	}
	switch entry.ref.kind {
	case payloadKind:
		if source, ok := t.backend.(FileStatBackend); ok {
			info, err := source.FileStat(entry.ref.hash, entry.ref.rel)
			if err != nil {
				return Entry{}, err
			}
			entry.Size, entry.CreatedAt = info.Size, info.CreatedAt
		}
	case subtitleKind:
		if source, ok := t.backend.(SubtitleStatBackend); ok {
			info, err := source.SubtitleStat(entry.ref.hash, entry.ref.rel)
			if err != nil {
				return Entry{}, err
			}
			entry.Size, entry.ModifiedAt = info.Size, info.ModifiedAt
		}
	}
	if entry.Size < 0 {
		return Entry{}, ErrInvalidName
	}
	return entry, nil
}

func (t *Tree) ReadDir(raw string) ([]Entry, error) {
	directory, err := t.OpenDir(raw)
	if err != nil {
		return nil, err
	}
	return directory.Entries(), nil
}

func (t *Tree) children(parent Entry) ([]Entry, error) {
	if !parent.IsDir {
		return nil, ErrNotDir
	}
	var out []Entry
	switch parent.ref.kind {
	case rootKind, categoryKind:
		observed := t.directories.observeListing()
		views := t.backend.Torrents()
		live := make(map[metainfo.Hash]bool, len(views))
		for _, view := range views {
			live[view.Hash] = true
		}
		t.finishDirectoryListing(observed, live)
		var categories []string
		if source, ok := t.backend.(CategoryBackend); ok {
			categories = source.Categories()
		}
		for _, item := range VisibleRootEntries(views, categories, parent.ref.category) {
			entry := Entry{Name: item.Name, Path: childPath(parent.Path, item.Name),
				Size: item.Size, CreatedAt: item.View.CreatedAt, ModifiedAt: item.ModifiedAt}
			switch {
			case item.IsCategory:
				entry.IsDir = true
				entry.ref = reference{kind: categoryKind, category: item.Name}
				entry.Identity = t.identity(categoryKey(item.Name))
			case item.IsSubtitle:
				entry.ref = reference{kind: subtitleKind, hash: item.View.Hash, rel: item.Path}
				entry.Identity = t.identity(fileKey(item.View.Hash, item.Path))
			default:
				if file, single := mediaRoot(item.View); single {
					entry.Size = file.Size
					entry.ref = reference{kind: payloadKind, hash: item.View.Hash, rel: file.Path}
					entry.Identity = t.identity(fileKey(item.View.Hash, file.Path))
				} else {
					entry.IsDir = true
					entry.ref = reference{kind: directoryKind, hash: item.View.Hash}
					entry.Identity = t.identity(torrentKey(item.View.Hash))
				}
			}
			out = append(out, entry)
		}
	case directoryKind:
		contents, index, err := t.directory(parent.ref.hash, parent.ref.rel)
		if err != nil {
			return nil, err
		}
		for _, item := range index.entries {
			out = append(out, t.layoutChild(parent, item, contents))
		}

	}
	return out, nil
}

func (t *Tree) layoutChild(parent Entry, item fsEntry, contents TorrentContents) Entry {
	entry := Entry{Name: item.Name, Path: childPath(parent.Path, item.Name), IsDir: item.IsDir,
		Size: item.Size, CreatedAt: contents.CreatedAt, ModifiedAt: item.ModifiedAt}
	if item.IsDir {
		rel := joinRel(parent.ref.rel, item.Name)
		entry.ref = reference{kind: directoryKind, hash: parent.ref.hash, rel: rel}
		entry.Identity = t.identity(dirKey(parent.ref.hash, rel))
	} else {
		kind := payloadKind
		if item.IsSubtitle {
			kind = subtitleKind
		}
		entry.ref = reference{kind: kind, hash: parent.ref.hash, rel: item.Path}
		entry.Identity = t.identity(fileKey(parent.ref.hash, item.Path))
	}
	return entry
}

// directory observes ownership before asking the source, so a late old
// contents result cannot recreate a version removed by a newer library scan.
func (t *Tree) directory(hash metainfo.Hash, prefix string) (TorrentContents, *directoryIndex, error) {
	observed := t.directories.observe(hash)
	defer t.directories.finish(hash, observed)
	contents, err := torrentContents(t.backend, hash)
	if err != nil || (contents.SingleFile && len(contents.Files) == 1) {
		t.forgetDirectory(hash, observed)
		if err == nil {
			err = ErrNotFound
		}
		return TorrentContents{}, nil, err
	}
	index := t.directoryIndex(hash, prefix, contents, observed)
	if prefix != "" && len(index.entries) == 0 {
		return TorrentContents{}, nil, ErrNotFound
	}
	return contents, index, nil
}

func (t *Tree) directoryIndex(hash metainfo.Hash, prefix string, contents TorrentContents, observed snapshotObservation[directoryVersion]) *directoryIndex {
	cache := &t.directories
	cache.mu.Lock()
	var retired *directoryVersion
	defer func() {
		cache.mu.Unlock()
		t.evictDirectoryVersion(retired)
	}()
	version := cache.currentLocked(hash)
	if version == nil || version.filesID != contents.FilesID ||
		(contents.FilesID == nil && !slices.Equal(version.files, contents.Files)) ||
		!slices.Equal(version.subtitles, contents.Subtitles) {
		files := contents.Files
		if contents.FilesID == nil {
			files = slices.Clone(files)
		}
		built := &directoryVersion{filesID: contents.FilesID, files: files,
			subtitles: slices.Clone(contents.Subtitles), prefixes: make(map[string]*directoryIndex)}
		if cache.publishLocked(hash, observed, built) {
			retired = version
		}
		version = built
	}
	if index := version.prefixes[prefix]; index != nil {
		return index
	}
	entries := childrenOfViews(version.files, version.subtitles, prefix)
	index := &directoryIndex{entries: entries, byName: make(map[string]fsEntry, len(entries)), owner: version}
	for _, entry := range entries {
		index.byName[entry.Name] = entry
	}
	if prefix == "" || len(entries) != 0 {
		version.prefixes[prefix] = index
	}
	return index
}

func (t *Tree) evictDirectoryVersion(version *directoryVersion) {
	if version == nil {
		return
	}
	t.directories.mu.Lock()
	snapshots := make([]*Directory, 0, len(version.prefixes))
	for _, index := range version.prefixes {
		if index.snapshot != nil {
			snapshots = append(snapshots, index.snapshot)
		}
	}
	t.directories.mu.Unlock()
	for _, snapshot := range snapshots {
		snapshot.evict()
	}
}

func (t *Tree) forgetDirectory(hash metainfo.Hash, observed snapshotObservation[directoryVersion]) {
	cache := &t.directories
	cache.mu.Lock()
	previous := cache.currentLocked(hash)
	cache.forgetLocked(hash, observed)
	removed := cache.currentLocked(hash) != previous
	cache.mu.Unlock()
	if removed {
		t.evictDirectoryVersion(previous)
	}
}

// Reuse the same already-requested root/category listing for pruning. There is
// no extra whole-library query on narrow lookups, and snapshotCache fences an
// older listing from evicting a newer published version.
func (t *Tree) finishDirectoryListing(observed *snapshotListing[directoryVersion], live map[metainfo.Hash]bool) {
	cache := &t.directories
	cache.mu.Lock()
	candidates := make(map[metainfo.Hash]*directoryVersion)
	for hash, slot := range cache.slots {
		if !live[hash] && slot.current != nil {
			candidates[hash] = slot.current
		}
	}
	cache.mu.Unlock()
	cache.finishListing(observed, live)
	cache.mu.Lock()
	var retired []*directoryVersion
	for hash, version := range candidates {
		if cache.currentLocked(hash) != version {
			retired = append(retired, version)
		}
	}
	cache.mu.Unlock()
	for _, version := range retired {
		t.evictDirectoryVersion(version)
	}
}

func childPath(parent, name string) string { return strings.TrimSuffix(parent, "/") + "/" + name }

// VisibleRootEntries is shared by protocol adapters. Managed subtitles of
// single-file torrents sit beside the file and can never shadow payloads.
func VisibleRootEntries(views []TorrentView, categories []string, category string) []RootEntry {
	var reserved map[string]bool
	if category == "" {
		reserved = categoryReservations(categories)
	}
	entries := rootEntriesForCategory(views, category, reserved)
	torrents := slices.Clone(entries)
	if category == "" {
		for _, name := range categories {
			entries = append(entries, rootEntry{Name: name, IsCategory: true})
		}
	}
	used := make(map[string]bool, len(entries))
	for _, entry := range entries {
		used[entry.Name] = true
	}
	for _, entry := range torrents {
		if _, single := mediaRoot(entry.View); !single {
			continue
		}
		for _, subtitle := range entry.View.Subtitles {
			if strings.Contains(subtitle.Path, "/") || used[subtitle.Path] {
				continue
			}
			used[subtitle.Path] = true
			entries = append(entries, rootEntry{Name: subtitle.Path, View: entry.View,
				Path: subtitle.Path, Size: subtitle.Size, ModifiedAt: subtitle.ModifiedAt, IsSubtitle: true})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries
}

type Directory struct {
	entry     Entry
	entries   []Entry
	version   uint64
	cacheMu   sync.Mutex
	cacheable bool
	cleanups  []func()
}

func (t *Tree) OpenDir(raw string) (*Directory, error) {
	entry, err := t.Lookup(raw)
	if err != nil {
		return nil, err
	}
	if !entry.IsDir {
		return nil, ErrNotDir
	}
	if entry.ref.kind != directoryKind {
		entries, err := t.children(entry)
		if err != nil {
			return nil, err
		}
		t.mu.Lock()
		t.nextVersion++
		version := t.nextVersion
		t.mu.Unlock()
		return &Directory{entry: entry, entries: entries, version: version}, nil
	}
	contents, index, err := t.directory(entry.ref.hash, entry.ref.rel)
	if err != nil {
		return nil, err
	}
	cache := &t.directories
	cache.mu.Lock()
	snapshot := index.snapshot
	if snapshot != nil && snapshot.entry.Path == entry.Path && snapshot.entry.CreatedAt.Equal(contents.CreatedAt) {
		owned := cache.currentLocked(entry.ref.hash) == index.owner
		cache.mu.Unlock()
		if !owned {
			snapshot.evict()
		}
		return snapshot, nil
	}
	cache.mu.Unlock()
	entries := make([]Entry, 0, len(index.entries))
	for _, item := range index.entries {
		entries = append(entries, t.layoutChild(entry, item, contents))
	}
	entry.CreatedAt = contents.CreatedAt
	cache.mu.Lock()
	if current := index.snapshot; current != nil && current.entry.Path == entry.Path && current.entry.CreatedAt.Equal(contents.CreatedAt) {
		owned := cache.currentLocked(entry.ref.hash) == index.owner
		cache.mu.Unlock()
		if !owned {
			current.evict()
		}
		return current, nil
	}
	t.mu.Lock()
	t.nextVersion++
	version := t.nextVersion
	t.mu.Unlock()
	snapshot = &Directory{entry: entry, entries: entries, version: version,
		cacheable: cache.currentLocked(entry.ref.hash) == index.owner}
	previous := index.snapshot
	index.snapshot = snapshot
	cache.mu.Unlock()
	if previous != nil {
		previous.evict()
	}
	return snapshot, nil
}

// Version changes with directory membership, metadata or its virtual parent
// path. Adapters may reuse name indexes while this immutable snapshot lives.
func (d *Directory) Version() uint64 { return d.version }

func (d *Directory) Metadata() Entry  { return d.entry }
func (d *Directory) Entries() []Entry { return slices.Clone(d.entries) }
func (d *Directory) Close() error     { return nil }

// OnCacheEviction ties an adapter's auxiliary index to this snapshot's cache
// ownership. A snapshot remains readable after eviction. A late subscriber is
// notified immediately; callbacks always run outside tree and snapshot locks.
func (d *Directory) OnCacheEviction(cleanup func()) {
	if cleanup == nil {
		return
	}
	d.cacheMu.Lock()
	if d.cacheable {
		d.cleanups = append(d.cleanups, cleanup)
		d.cacheMu.Unlock()
		return
	}
	d.cacheMu.Unlock()
	cleanup()
}

func (d *Directory) evict() {
	d.cacheMu.Lock()
	d.cacheable = false
	cleanups := d.cleanups
	d.cleanups = nil
	d.cacheMu.Unlock()
	for _, cleanup := range cleanups {
		cleanup()
	}
}

type Handle struct {
	entry  Entry
	reader io.ReaderAt
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.RWMutex
	closed bool
	once   sync.Once
	err    error
}

func (t *Tree) Open(raw string) (*Handle, error) {
	entry, err := t.Lookup(raw)
	if err != nil {
		return nil, err
	}
	if entry.IsDir {
		return nil, ErrIsDir
	}
	var reader io.ReaderAt
	if entry.ref.kind == subtitleKind {
		source, ok := t.backend.(SubtitleBackend)
		if !ok {
			return nil, ErrNotFound
		}
		snapshot, openErr := source.OpenSubtitle(entry.ref.hash, entry.ref.rel)
		err, reader = openErr, snapshot.Reader
		entry.Size, entry.ModifiedAt = snapshot.Size, snapshot.ModifiedAt
	} else if source, ok := t.backend.(FileSnapshotBackend); ok {
		snapshot, openErr := source.OpenFileSnapshot(entry.ref.hash, entry.ref.rel)
		err, reader = openErr, snapshot.Reader
		entry.Size, entry.CreatedAt = snapshot.Size, snapshot.CreatedAt
	} else {
		reader, err = t.backend.OpenFile(entry.ref.hash, entry.ref.rel)
	}
	if err != nil {
		return nil, err
	}
	if reader == nil || entry.Size < 0 {
		if closer, ok := reader.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, ErrInvalidName
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Handle{entry: entry, reader: reader, ctx: ctx, cancel: cancel}, nil
}

func (h *Handle) Metadata() Entry { return h.entry }
func (h *Handle) ReadAt(p []byte, off int64) (int, error) {
	return h.ReadAtContext(context.Background(), p, off)
}

func (h *Handle) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed {
		return 0, fs.ErrClosed
	}
	if off < 0 {
		return 0, ErrInvalidName
	}
	readCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(h.ctx, cancel)
	defer cancel()
	defer stop()
	if err := readCtx.Err(); err != nil {
		return 0, err
	}
	if reader, ok := h.reader.(interface {
		ReadAtContext(context.Context, []byte, int64) (int, error)
	}); ok {
		return reader.ReadAtContext(readCtx, p, off)
	}
	return h.reader.ReadAt(p, off)
}

func (h *Handle) Close() error {
	h.once.Do(func() {
		h.cancel()
		h.mu.Lock()
		defer h.mu.Unlock()
		h.closed = true
		if closer, ok := h.reader.(io.Closer); ok {
			h.err = closer.Close()
		}
	})
	return h.err
}
