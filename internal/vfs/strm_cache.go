package vfs

import (
	"io"
	"slices"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

type strmBackend struct {
	backend StreamURLBackend
	cache   snapshotCache[strmTorrent]

	mappingBuilds uint64 // guarded by cache.mu
}

// strmTorrent owns one immutable path mapping, keyed by source identity and
// SingleFile shape. Legacy payloads are compared by value and privately copied.
// ready deduplicates construction; mutable projection metadata uses cache.mu.
type strmTorrent struct {
	payload    []FileView
	sourceID   *FileListID
	singleFile bool
	files      []strmFile
	byPath     map[string]int
	ready      chan struct{}

	textSizes   []strmTextSize
	createdAt   time.Time
	projected   []FileView
	projectedID *FileListID
	complete    bool
	projecting  chan struct{}
}

// A size revision belongs to one file, not the torrent: concurrent observations
// of different files must not invalidate each other's successful backfills.
type strmTextSize struct {
	size int64
	id   *FileListID
}

func (e *strmTorrent) matches(view TorrentView) bool {
	if e.singleFile != view.SingleFile {
		return false
	}
	if view.FilesID != nil {
		return e.sourceID == view.FilesID
	}
	return e.sourceID == nil && slices.Equal(e.payload, view.Files)
}

func (b *strmBackend) Torrents() []TorrentView {
	observed := b.cache.observeListing()
	views := b.backend.Torrents()
	projected := slices.Clone(views) // Root naming must never modify source views.
	live := make(map[metainfo.Hash]bool, len(views))
	defer func() { b.cache.finishListing(observed, live) }()
	for i, view := range views {
		live[view.Hash] = true
		contents := b.projectContents(view, observed.observation(view.Hash))
		projected[i].Files, projected[i].FilesID = contents.Files, contents.FilesID
		projected[i].Subtitles = contents.Subtitles
	}
	var categories []string
	if provider, ok := b.backend.(CategoryBackend); ok {
		categories = provider.Categories()
	}
	strmRootNames(views, projected, categories)
	return projected
}

// TorrentContents projects just one current torrent, without root naming or a
// whole-library enumeration when the source supports the narrow interface.
func (b *strmBackend) TorrentContents(hash metainfo.Hash) (TorrentContents, error) {
	observed := b.cache.observe(hash)
	defer b.cache.finish(hash, observed)
	contents, err := torrentContents(b.backend, hash)
	if err != nil {
		b.cache.forget(hash, observed)
		return TorrentContents{}, err
	}
	return b.projectContents(viewOfContents(hash, contents), observed), nil
}

func viewOfContents(hash metainfo.Hash, contents TorrentContents) TorrentView {
	return TorrentView{Hash: hash, Files: contents.Files, FilesID: contents.FilesID,
		Subtitles: contents.Subtitles, CreatedAt: contents.CreatedAt, SingleFile: contents.SingleFile}
}

func (b *strmBackend) projectContents(view TorrentView, observed snapshotObservation[strmTorrent]) TorrentContents {
	entry := b.projectTorrent(view, observed)
	files, id := b.projectedFiles(view.Hash, entry)
	return TorrentContents{Files: files, FilesID: id, Subtitles: slices.Clone(view.Subtitles),
		CreatedAt: view.CreatedAt, SingleFile: view.SingleFile}
}

// projectTorrent is the root-name-independent mapping path shared by listings,
// narrow contents reads, and cold payload lookups. Publish a construction slot
// before doing expensive work, so concurrent first readers build it only once.
func (b *strmBackend) projectTorrent(view TorrentView, observed snapshotObservation[strmTorrent]) *strmTorrent {
	b.cache.mu.Lock()
	if entry := b.cache.currentLocked(view.Hash); entry != nil && entry.matches(view) {
		b.refreshCreatedAtLocked(view, entry, observed)
		b.cache.mu.Unlock()
		<-entry.ready
		return entry
	}
	b.cache.mu.Unlock()

	payload := view.Files
	if view.FilesID == nil {
		payload = slices.Clone(payload)
	}
	built := &strmTorrent{payload: payload, sourceID: view.FilesID, singleFile: view.SingleFile,
		createdAt: view.CreatedAt, ready: make(chan struct{})}
	b.cache.mu.Lock()
	if entry := b.cache.currentLocked(view.Hash); entry != nil && entry.matches(view) {
		b.refreshCreatedAtLocked(view, entry, observed)
		b.cache.mu.Unlock()
		<-entry.ready
		return entry
	}
	// If another request has superseded this observation, the old request may
	// finish its own snapshot, but must not replace or resurrect the cache.
	b.cache.publishLocked(view.Hash, observed, built)
	b.mappingBuilds++
	b.cache.mu.Unlock()

	view.Files = payload
	built.files = strmFiles(view)
	built.byPath = make(map[string]int, len(built.files))
	for i, file := range built.files {
		if _, exists := built.byPath[file.path]; !exists {
			built.byPath[file.path] = i
		}
	}
	built.textSizes = make([]strmTextSize, len(built.files))
	close(built.ready)
	return built
}

func (b *strmBackend) refreshCreatedAtLocked(view TorrentView, entry *strmTorrent, observed snapshotObservation[strmTorrent]) {
	if b.cache.unchangedLocked(view.Hash, observed) && entry.createdAt != view.CreatedAt {
		entry.createdAt = view.CreatedAt
		// The path mapping is unchanged, but this is a newer metadata
		// observation. Fence old readers without rebuilding the mapping.
		b.cache.slots[view.Hash] = &snapshotSlot[strmTorrent]{current: entry}
	}
}

// projectedFiles retries each unavailable signature at most once per call.
// Waiters share the completed attempt, including its failures, instead of
// immediately retrying it in a loop. Complete trusted projections are read-only.
func (b *strmBackend) projectedFiles(hash metainfo.Hash, entry *strmTorrent) ([]FileView, *FileListID) {
	b.cache.mu.Lock()
	if entry.complete {
		files, id := b.projectionLocked(entry)
		b.cache.mu.Unlock()
		return files, id
	}
	if pending := entry.projecting; pending != nil {
		b.cache.mu.Unlock()
		<-pending
		b.cache.mu.Lock()
		files, id := b.projectionLocked(entry)
		b.cache.mu.Unlock()
		return files, id
	}
	pending := make(chan struct{})
	entry.projecting = pending
	var missing []int
	for i, file := range entry.files {
		if file.generated && entry.textSizes[i].size == 0 {
			missing = append(missing, i)
		}
	}
	b.cache.mu.Unlock()

	signed := make(map[int]int64, len(missing))
	for _, i := range missing {
		if link, err := b.backend.StreamURL(hash, entry.files[i].original); err == nil {
			signed[i] = int64(len(link) + 1)
		}
	}
	b.cache.mu.Lock()
	defer b.cache.mu.Unlock()
	var revision *FileListID
	for i, size := range signed {
		// A concurrent live stat may already have filled in a newer size.
		if entry.textSizes[i].size == 0 {
			if revision == nil {
				revision = &FileListID{}
			}
			entry.textSizes[i] = strmTextSize{size: size, id: revision}
		}
	}
	if revision != nil {
		entry.projected = nil
	}
	files, id := b.projectionLocked(entry)
	entry.projecting = nil
	close(pending)
	return files, id
}

// projectionLocked publishes a fresh slice after any size change. It never
// edits a slice previously returned with a derived identity.
func (b *strmBackend) projectionLocked(entry *strmTorrent) ([]FileView, *FileListID) {
	if entry.projected == nil {
		files := make([]FileView, len(entry.files))
		complete := true
		for i, file := range entry.files {
			size := entry.payload[i].Size
			if file.generated {
				size = entry.textSizes[i].size
				complete = complete && size != 0
			}
			files[i] = FileView{Path: file.path, Size: size}
		}
		entry.projected, entry.complete = files, complete
		entry.projectedID = nil
		if complete && entry.sourceID != nil {
			entry.projectedID = &FileListID{}
		}
	}
	if entry.projectedID != nil {
		return entry.projected, entry.projectedID
	}
	return slices.Clone(entry.projected), nil
}

func (b *strmBackend) OpenFile(hash metainfo.Hash, path string) (io.ReaderAt, error) {
	snapshot, err := b.OpenFileSnapshot(hash, path)
	return snapshot.Reader, err
}

type strmPayload struct {
	file   strmFile
	stat   FileStat
	entry  *strmTorrent
	index  int
	sizeID *FileListID
}

func (b *strmBackend) FileStat(hash metainfo.Hash, path string) (FileStat, error) {
	payload, err := b.payloadFile(hash, path)
	if err != nil {
		return FileStat{}, err
	}
	if payload.file.generated {
		// Always validate registration/readiness, without opening the video.
		link, err := b.backend.StreamURL(hash, payload.file.original)
		if err != nil {
			return FileStat{}, err
		}
		payload.stat.Size = int64(len(link) + 1)
		b.rememberTextSize(hash, payload, payload.stat.Size)
	}
	return payload.stat, nil
}

func (b *strmBackend) rememberTextSize(hash metainfo.Hash, payload strmPayload, size int64) {
	b.cache.mu.Lock()
	defer b.cache.mu.Unlock()
	entry := b.cache.currentLocked(hash)
	if entry != payload.entry || entry.textSizes[payload.index].id != payload.sizeID || entry.textSizes[payload.index].size == size {
		return
	}
	entry.textSizes[payload.index] = strmTextSize{size: size, id: &FileListID{}}
	entry.projected, entry.projectedID, entry.complete = nil, nil, false
}

func (b *strmBackend) OpenFileSnapshot(hash metainfo.Hash, path string) (FileSnapshot, error) {
	payload, err := b.payloadFile(hash, path)
	if err != nil {
		return FileSnapshot{}, err
	}
	if payload.file.generated {
		// The same live URL supplies the reader and its bound, even if an old
		// inode still remembers a failed listing's zero size.
		link, err := b.backend.StreamURL(hash, payload.file.original)
		if err != nil {
			return FileSnapshot{}, err
		}
		text := link + "\n"
		b.rememberTextSize(hash, payload, int64(len(text)))
		return FileSnapshot{Reader: strings.NewReader(text), Size: int64(len(text)), CreatedAt: payload.stat.CreatedAt}, nil
	}
	reader, err := b.backend.OpenFile(hash, payload.file.original)
	if err != nil {
		return FileSnapshot{}, err
	}
	if reader == nil {
		return FileSnapshot{}, ErrInvalidName
	}
	return FileSnapshot{Reader: reader, Size: payload.stat.Size, CreatedAt: payload.stat.CreatedAt}, nil
}

// payloadFile keeps the deletion semantics of cached payload attributes: until
// a listing or narrow miss removes the entry, passthrough stat may succeed from
// that immutable mapping, but opening still asks the live backend and fails.
// An inode outliving the entry uses one current source snapshot to refill it.
func (b *strmBackend) payloadFile(hash metainfo.Hash, path string) (strmPayload, error) {
	if payload, ok := b.cachedPayloadFile(hash, path); ok {
		if payload.stat.Size < 0 {
			return strmPayload{}, ErrInvalidName
		}
		return payload, nil
	}
	observed := b.cache.observe(hash)
	defer b.cache.finish(hash, observed)
	contents, err := torrentContents(b.backend, hash)
	if err != nil {
		b.cache.forget(hash, observed)
		return strmPayload{}, err
	}
	entry := b.projectTorrent(viewOfContents(hash, contents), observed)
	b.cache.mu.Lock()
	payload, ok := payloadInEntry(entry, path)
	b.cache.mu.Unlock()
	if !ok {
		return strmPayload{}, ErrNotFound
	}
	if payload.stat.Size < 0 {
		return strmPayload{}, ErrInvalidName
	}
	return payload, nil
}

func (b *strmBackend) cachedPayloadFile(hash metainfo.Hash, path string) (strmPayload, bool) {
	b.cache.mu.Lock()
	entry := b.cache.currentLocked(hash)
	b.cache.mu.Unlock()
	if entry == nil {
		return strmPayload{}, false
	}
	<-entry.ready
	b.cache.mu.Lock()
	defer b.cache.mu.Unlock()
	return payloadInEntry(entry, path)
}

func payloadInEntry(entry *strmTorrent, path string) (strmPayload, bool) {
	i, ok := entry.byPath[path]
	if !ok {
		return strmPayload{}, false
	}
	return strmPayload{file: entry.files[i], stat: FileStat{Size: entry.payload[i].Size, CreatedAt: entry.createdAt},
		entry: entry, index: i, sizeID: entry.textSizes[i].id}, true
}
