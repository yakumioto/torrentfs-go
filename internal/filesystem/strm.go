package filesystem

import (
	"crypto/sha256"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/media"
)

// StreamURLBackend supplies signed URLs for the ready payload files it exposes
// through Backend. Neither building a URL nor listing files reads payload data.
// If a URL is unavailable, directory snapshots keep that file with Size zero.
// Live payload stat and open check StreamURL again and return its error instead
// of exposing an unavailable file as successfully readable empty content.
type StreamURLBackend interface {
	Backend
	StreamURL(hash metainfo.Hash, path string) (string, error)
}

// NewSTRMBackend projects videos as in-memory STRM files while preserving the
// original backend's optional category and managed-subtitle capabilities.
func NewSTRMBackend(backend StreamURLBackend) Backend {
	projected := &strmBackend{backend: backend}
	categories, hasCategories := backend.(CategoryBackend)
	subtitles, hasSubtitles := backend.(SubtitleBackend)
	stat, hasStat := backend.(SubtitleStatBackend)
	// Preserve the absence of optional interfaces too: in particular, adding a
	// SubtitleStat method would bypass a node's lookup-time metadata fallback.
	switch {
	case hasCategories && hasSubtitles && hasStat:
		return &struct {
			*strmBackend
			CategoryBackend
			SubtitleBackend
			SubtitleStatBackend
		}{projected, categories, subtitles, stat}
	case hasCategories && hasSubtitles:
		return &struct {
			*strmBackend
			CategoryBackend
			SubtitleBackend
		}{projected, categories, subtitles}
	case hasCategories && hasStat:
		return &struct {
			*strmBackend
			CategoryBackend
			SubtitleStatBackend
		}{projected, categories, stat}
	case hasSubtitles && hasStat:
		return &struct {
			*strmBackend
			SubtitleBackend
			SubtitleStatBackend
		}{projected, subtitles, stat}
	case hasCategories:
		return &struct {
			*strmBackend
			CategoryBackend
		}{projected, categories}
	case hasSubtitles:
		return &struct {
			*strmBackend
			SubtitleBackend
		}{projected, subtitles}
	case hasStat:
		return &struct {
			*strmBackend
			SubtitleStatBackend
		}{projected, stat}
	default:
		return projected
	}
}

type strmBackend struct {
	backend StreamURLBackend

	mu       sync.Mutex
	torrents map[metainfo.Hash]*strmTorrent
}

// strmTorrent caches one torrent's projection. Every FUSE lookup reads the
// backend's whole-library snapshot, so naming each torrent and signing each
// video again there would make a large library quadratic. The mapping is
// reused only while the payload list is identical; text sizes are filled in
// as URLs are generated, and createdAt follows the latest listing.
type strmTorrent struct {
	payload []FileView
	files   []strmFile
	byPath  map[string]int

	// textSizes (URL plus newline, zero until StreamURL succeeds), createdAt and
	// the completed projection are guarded by strmBackend.mu.
	textSizes []int64
	createdAt time.Time
	projected []FileView
}

func newSTRMTorrent(view TorrentView) *strmTorrent {
	files := strmFiles(view)
	byPath := make(map[string]int, len(files))
	for i, file := range files {
		// Keep the first of duplicated display paths, as a linear scan would.
		if _, ok := byPath[file.path]; !ok {
			byPath[file.path] = i
		}
	}
	return &strmTorrent{
		payload:   slices.Clone(view.Files),
		files:     files,
		byPath:    byPath,
		textSizes: make([]int64, len(files)),
		createdAt: view.CreatedAt,
	}
}

func (b *strmBackend) Torrents() []TorrentView {
	views := b.backend.Torrents()
	projected := slices.Clone(views)
	live := make(map[metainfo.Hash]bool, len(views))
	for i, view := range views {
		live[view.Hash] = true
		projected[i].Files = b.projectedFiles(view.Hash, b.entry(view))
		projected[i].Subtitles = slices.Clone(view.Subtitles)
	}
	b.mu.Lock()
	for hash := range b.torrents {
		if !live[hash] {
			delete(b.torrents, hash)
		}
	}
	b.mu.Unlock()
	var categories []string
	if provider, ok := b.backend.(CategoryBackend); ok {
		categories = provider.Categories()
	}
	strmRootNames(views, projected, categories)
	return projected
}

// entry returns the cached projection for view, building it outside the lock.
func (b *strmBackend) entry(view TorrentView) *strmTorrent {
	b.mu.Lock()
	if entry := b.torrents[view.Hash]; entry != nil && slices.Equal(entry.payload, view.Files) {
		entry.createdAt = view.CreatedAt
		b.mu.Unlock()
		return entry
	}
	b.mu.Unlock()
	built := newSTRMTorrent(view)
	b.mu.Lock()
	defer b.mu.Unlock()
	if entry := b.torrents[view.Hash]; entry != nil && slices.Equal(entry.payload, view.Files) {
		entry.createdAt = view.CreatedAt
		return entry
	}
	if b.torrents == nil {
		b.torrents = make(map[metainfo.Hash]*strmTorrent)
	}
	b.torrents[view.Hash] = built
	return built
}

// projectedFiles signs only videos whose text size is still unknown. A failure
// keeps the generated identity with Size zero (there is no readable text to
// size), is not cached, and is retried by the next listing; OpenFile and live
// stat return the error instead of exposing empty content.
func (b *strmBackend) projectedFiles(hash metainfo.Hash, entry *strmTorrent) []FileView {
	b.mu.Lock()
	if entry.projected != nil {
		files := slices.Clone(entry.projected)
		b.mu.Unlock()
		return files
	}
	var missing []int
	for i, file := range entry.files {
		if file.generated && entry.textSizes[i] == 0 {
			missing = append(missing, i)
		}
	}
	b.mu.Unlock()

	signed := make(map[int]int64, len(missing))
	for _, i := range missing {
		if url, err := b.backend.StreamURL(hash, entry.files[i].original); err == nil {
			signed[i] = int64(len(url) + 1)
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	for i, size := range signed {
		entry.textSizes[i] = size
	}
	files := make([]FileView, len(entry.files))
	complete := true
	for i, file := range entry.files {
		size := entry.payload[i].Size
		if file.generated {
			size = entry.textSizes[i]
			complete = complete && size != 0
		}
		files[i] = FileView{Path: file.path, Size: size}
	}
	if complete {
		entry.projected = slices.Clone(files)
	}
	return files
}

func (b *strmBackend) OpenFile(hash metainfo.Hash, path string) (io.ReaderAt, error) {
	snapshot, err := b.OpenFileSnapshot(hash, path)
	return snapshot.Reader, err
}

func (b *strmBackend) FileStat(hash metainfo.Hash, path string) (FileStat, error) {
	file, stat, err := b.payloadFile(hash, path)
	if err != nil {
		return FileStat{}, err
	}
	if file.generated {
		// One signature checks registration and readiness for this payload;
		// it never lists the library or opens the video.
		url, err := b.backend.StreamURL(hash, file.original)
		if err != nil {
			return FileStat{}, err
		}
		stat.Size = int64(len(url) + 1)
		b.rememberTextSize(hash, path, stat.Size)
	}
	return stat, nil
}

func (b *strmBackend) rememberTextSize(hash metainfo.Hash, path string, size int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if entry := b.torrents[hash]; entry != nil {
		if i, ok := entry.byPath[path]; ok && entry.textSizes[i] == 0 {
			entry.textSizes[i] = size
		}
	}
}

func (b *strmBackend) OpenFileSnapshot(hash metainfo.Hash, path string) (FileSnapshot, error) {
	file, stat, err := b.payloadFile(hash, path)
	if err != nil {
		return FileSnapshot{}, err
	}
	if file.generated {
		// Registration and readiness are checked on every open. The exact same
		// generated text supplies both the reader and its read bound, including
		// when an older inode still holds an unavailable listing's zero size.
		url, err := b.backend.StreamURL(hash, file.original)
		if err != nil {
			return FileSnapshot{}, err
		}
		text := url + "\n"
		return FileSnapshot{Reader: strings.NewReader(text), Size: int64(len(text)), CreatedAt: stat.CreatedAt}, nil
	}
	reader, err := b.backend.OpenFile(hash, file.original)
	if err != nil {
		return FileSnapshot{}, err
	}
	if reader == nil {
		return FileSnapshot{}, ErrInvalidName
	}
	return FileSnapshot{Reader: reader, Size: stat.Size, CreatedAt: stat.CreatedAt}, nil
}

// payloadFile maps a projected path back to its payload. Listings cache the
// mapping for every live torrent, so a lookup needs no library snapshot. An
// inode can outlive that entry (its torrent was removed, possibly re-added);
// only then is the backend's snapshot consulted again. Until the next listing
// drops a removed torrent, a passthrough file's stat still succeeds from the
// cache, as the original view's cached attributes do; opening it fails.
func (b *strmBackend) payloadFile(hash metainfo.Hash, path string) (strmFile, FileStat, error) {
	file, stat, ok := b.cachedPayloadFile(hash, path)
	if !ok {
		for _, view := range b.backend.Torrents() {
			if view.Hash == hash {
				b.entry(view)
				file, stat, ok = b.cachedPayloadFile(hash, path)
				break
			}
		}
	}
	if !ok {
		return strmFile{}, FileStat{}, ErrNotFound
	}
	if stat.Size < 0 {
		return strmFile{}, FileStat{}, ErrInvalidName
	}
	return file, stat, nil
}

func (b *strmBackend) cachedPayloadFile(hash metainfo.Hash, path string) (strmFile, FileStat, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := b.torrents[hash]
	if entry == nil {
		return strmFile{}, FileStat{}, false
	}
	i, ok := entry.byPath[path]
	if !ok {
		return strmFile{}, FileStat{}, false
	}
	return entry.files[i], FileStat{Size: entry.payload[i].Size, CreatedAt: entry.createdAt}, true
}

type strmFile struct {
	original  string
	path      string
	generated bool
}

// strmFiles deliberately ignores names, categories and managed subtitles. They
// can change while a payload inode remains alive; payload paths cannot.
func strmFiles(view TorrentView) []strmFile {
	files := make([]strmFile, len(view.Files))
	_, single := mediaRoot(view)
	usedByDir := make(map[string]map[string]bool)
	namesByDir := make(map[string][]strmName)
	reserve := func(dir, name string) {
		if usedByDir[dir] == nil {
			usedByDir[dir] = make(map[string]bool)
		}
		usedByDir[dir][name] = true
	}
	for i, file := range view.Files {
		ext, disc := strmExtension(file.Path)
		files[i] = strmFile{original: file.Path, path: file.Path, generated: ext != ""}
		if single {
			// rootNode displays View.Name but opens File.Path. Keep that
			// identity independent of root-level name and category collisions.
			continue
		}
		parts := strings.Split(file.Path, "/")
		dir := ""
		for _, part := range parts[:len(parts)-1] {
			reserve(dir, part)
			dir = joinRel(dir, part)
		}
		name := parts[len(parts)-1]
		if ext == "" {
			reserve(dir, name)
			continue
		}
		namesByDir[dir] = append(namesByDir[dir], strmName{
			index: i,
			key:   file.Path,
			stem:  strings.TrimSuffix(name, ext),
			ext:   ext,
			disc:  disc,
			hash:  fmt.Sprintf("%x", sha256.Sum256([]byte(file.Path))),
		})
	}
	for dir, names := range namesByDir {
		for _, name := range allocateSTRMNames(names, usedByDir[dir]) {
			files[name.index].path = joinRel(dir, name.name)
		}
	}
	return files
}

// strmRootNames first obtains the original layout's visible names, including
// its infohash disambiguation. Preallocating every projected root name keeps
// the existing nodes from appending a hash after the .strm extension.
func strmRootNames(views, projected []TorrentView, categories []string) {
	indices := make(map[metainfo.Hash]int, len(views))
	groups := make(map[string]bool)
	for i, view := range views {
		indices[view.Hash] = i
		groups[view.Category] = true
	}
	for category := range groups {
		var reserved map[string]bool
		if category == "" {
			reserved = categoryReservations(categories)
		}
		entries := rootEntriesForCategory(views, category, reserved)
		used := make(map[string]bool, len(entries)+len(reserved))
		for name := range reserved {
			used[name] = true
		}
		var names []strmName
		for _, entry := range entries {
			view := entry.View
			i := indices[view.Hash]
			projected[i].Name = entry.Name
			file, single := mediaRoot(view)
			ext, disc := strmExtension(file.Path)
			if !single || ext == "" {
				used[entry.Name] = true
			} else {
				stem := view.Name
				if visibleExt, _ := strmExtension(stem); visibleExt != "" {
					stem = strings.TrimSuffix(stem, visibleExt)
				}
				names = append(names, strmName{
					index: i,
					key:   view.Hash.HexString(),
					stem:  stem + strings.TrimPrefix(entry.Name, view.Name),
					ext:   ext,
					disc:  disc,
					hash:  view.Hash.HexString(),
				})
			}
			if single {
				for _, subtitle := range view.Subtitles {
					if !strings.Contains(subtitle.Path, "/") {
						used[subtitle.Path] = true
					}
				}
			}
		}
		for _, name := range allocateSTRMNames(names, used) {
			projected[name.index].Name = name.name
		}
	}
}

func strmExtension(name string) (string, bool) {
	if ext, ok := media.VideoExtension(name); ok {
		return ext, false
	}
	if media.IsDiscImage(name) {
		return name[len(name)-len(".iso"):], true
	}
	return "", false
}

type strmName struct {
	index int
	key   string
	stem  string
	ext   string
	disc  bool
	hash  string
	name  string
}

func (n strmName) ending() string {
	if n.disc {
		return ".iso.strm"
	}
	return ".strm"
}

// allocateSTRMNames reserves passthrough files and directories before assigning
// video names. Assignment order and collision suffixes depend only on the input
// identities, never on map iteration or a previous directory listing.
func allocateSTRMNames(names []strmName, used map[string]bool) []strmName {
	if used == nil {
		used = make(map[string]bool)
	}
	preferred := make(map[string]int, len(names))
	for i := range names {
		// A generated leaf must never introduce a path separator or NUL, even
		// when a backend supplies an unsafe torrent display name.
		names[i].stem = strings.NewReplacer("/", "_", "\\", "_", "\x00", "_").Replace(strings.ToValidUTF8(names[i].stem, "_"))
		n := names[i]
		preferred[fitSTRMName(n.stem, n.ending(), "", n.hash)]++
	}
	slices.SortStableFunc(names, func(a, b strmName) int {
		return strings.Compare(a.key, b.key)
	})
	for i := range names {
		n := &names[i]
		base := n.stem
		name := fitSTRMName(base, n.ending(), "", n.hash)
		if !n.disc && (preferred[name] > 1 || used[name]) {
			base += n.ext
			name = fitSTRMName(base, n.ending(), "", n.hash)
		}
		if used[name] {
			for length := 8; length <= len(n.hash); length += 4 {
				name = fitSTRMName(base, n.ending(), "-"+n.hash[:length], n.hash)
				if !used[name] {
					break
				}
			}
			for sequence := 1; used[name]; sequence++ {
				name = fitSTRMName(base, n.ending(), fmt.Sprintf("-%s-%d", n.hash, sequence), n.hash)
			}
		}
		n.name = name
		used[name] = true
	}
	return names
}

// fitSTRMName limits new leaf names to Linux NAME_MAX bytes, not runes. Long
// names retain both a hash and the complete STRM (or ISO STRM) ending.
func fitSTRMName(base, ending, suffix, hash string) string {
	const maxNameBytes = 255
	if suffix == "" && len(base)+len(ending) > maxNameBytes {
		suffix = "-" + hash[:8]
	}
	limit := maxNameBytes - len(ending) - len(suffix)
	if len(base) > limit {
		base = base[:limit]
		for !utf8.ValidString(base) {
			base = base[:len(base)-1]
		}
	}
	return base + suffix + ending
}

var (
	_ Backend             = (*strmBackend)(nil)
	_ FileStatBackend     = (*strmBackend)(nil)
	_ FileSnapshotBackend = (*strmBackend)(nil)
)
