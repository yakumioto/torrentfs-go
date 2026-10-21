// Package filesystem adapts torrent session data to a read-only FUSE filesystem.
package filesystem

import (
	"io"
	"sync"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// FileView describes one file of a torrent, addressed by its path relative to
// the torrent's top directory (the anacrolix "display path").
type FileView struct {
	Path string
	Size int64
}

// SubtitleView describes one managed subtitle projected beside the immutable
// torrent payload. Path is relative to the torrent's virtual root.
type SubtitleView struct {
	Path       string
	VideoPath  string
	Size       int64
	ModifiedAt time.Time
}

// SubtitleStat is one managed subtitle's current metadata.
type SubtitleStat struct {
	Size       int64
	ModifiedAt time.Time
}

// FileListID is an opaque identity for a permanently immutable payload list.
// A backend must publish a new slice and identity whenever any path or size
// changes. Its non-zero size gives separately allocated identities distinct
// addresses. A nil identity opts out of sharing and keeps legacy copy semantics.
type FileListID struct{ _ byte }

// TorrentContents is one torrent's current directory snapshot, independent of
// root naming and category layout. Only Files with a non-nil FilesID may be
// shared indefinitely; all other fields describe this particular observation.
type TorrentContents struct {
	Files      []FileView
	FilesID    *FileListID
	Subtitles  []SubtitleView
	CreatedAt  time.Time
	SingleFile bool
}

// TorrentContentsBackend optionally resolves one ready torrent without listing
// the library. Missing torrents and closed backends must return an error, never
// a previous snapshot.
type TorrentContentsBackend interface {
	TorrentContents(hash metainfo.Hash) (TorrentContents, error)
}

// TorrentView is the read-only snapshot of a torrent exposed to the
// filesystem layer. Files is non-empty once the torrent's metainfo is known.
// SingleFile marks a torrent whose metainfo has no directory structure
// (metainfo.Info.IsDir() is false): its one file is exposed directly as a
// regular file at the mount root instead of inside a directory. The zero
// value keeps the directory layout, so a Backend that never sets it still gets
// the historical behaviour.
type TorrentView struct {
	Name string
	Hash metainfo.Hash
	// Category is the optional top-level directory containing this torrent.
	Category string
	// CreatedAt is the durable time when the torrent was added to the session.
	CreatedAt time.Time
	Files     []FileView
	// FilesID authorizes read-only sharing of Files; nil retains legacy copying.
	FilesID    *FileListID
	Subtitles  []SubtitleView
	SingleFile bool
}

func setCreatedAt(attr *fuse.Attr, createdAt time.Time) {
	if createdAt.IsZero() {
		return
	}
	attr.SetTimes(&createdAt, &createdAt, &createdAt)
}

func setModifiedAt(attr *fuse.Attr, modifiedAt time.Time) {
	if modifiedAt.IsZero() {
		return
	}
	attr.SetTimes(&modifiedAt, &modifiedAt, &modifiedAt)
}

// Backend supplies the filesystem layer with torrent snapshots and file
// handles.
type Backend interface {
	// Torrents returns the torrents to expose. Only torrents whose metainfo
	// is available are included.
	Torrents() []TorrentView
	// OpenFile returns a handle for reading the file at the given display path
	// inside the torrent identified by hash.
	OpenFile(hash metainfo.Hash, path string) (io.ReaderAt, error)
}

// FileStat is one payload file's current metadata, including a generated file
// whose readable size can differ from an earlier directory snapshot.
type FileStat struct {
	Size      int64
	CreatedAt time.Time
}

// FileSnapshot binds an opened payload reader to the size of the same content.
type FileSnapshot struct {
	Reader    io.ReaderAt
	Size      int64
	CreatedAt time.Time
}

// FileStatBackend optionally refreshes payload metadata at lookup and getattr.
// Backends without it keep the directory snapshot's immutable payload metadata.
type FileStatBackend interface {
	FileStat(hash metainfo.Hash, path string) (FileStat, error)
}

// FileSnapshotBackend optionally supplies a reader and its metadata in one open,
// so a generated file cannot inherit an unavailable snapshot's zero read bound.
type FileSnapshotBackend interface {
	OpenFileSnapshot(hash metainfo.Hash, path string) (FileSnapshot, error)
}

// CategoryBackend is an optional Backend extension that supplies every
// persisted category, including categories with no visible torrents.
type CategoryBackend interface {
	Categories() []string
}

// SubtitleSnapshot is one opened managed subtitle: the file itself plus the
// metadata read from that same open. Size and ModifiedAt therefore always
// describe the bytes the Reader serves, even if the subtitle is replaced while
// the open is in flight.
type SubtitleSnapshot struct {
	Reader     io.ReaderAt
	Size       int64
	ModifiedAt time.Time
}

// SubtitleBackend is the optional extension used for managed subtitle files.
// Keeping it separate preserves compatibility with small payload-only backends.
type SubtitleBackend interface {
	OpenSubtitle(hash metainfo.Hash, path string) (SubtitleSnapshot, error)
}

// SubtitleStatBackend is the optional extension that reports a managed
// subtitle's live metadata. A FUSE inode is cached by path, so a node that
// copied its size and mtime at lookup time would keep answering with the old
// file's metadata after a replacement; asking the backend on every attribute
// read and open is what makes a changed length visible. Backends that do not
// implement it keep serving the lookup-time snapshot.
type SubtitleStatBackend interface {
	SubtitleStat(hash metainfo.Hash, path string) (SubtitleStat, error)
}

// fsState carries the pieces shared by every node in a mount: the Backend and
// the stable inode allocator.
type fsState struct {
	backend Backend

	// Membership indexes are mount-private and independent of inode allocation.
	views           snapshotCache[torrentDirectoryVersion]
	directoryBuilds uint64 // guarded by views.mu

	mu       sync.Mutex
	nextIno  uint64
	inoByKey map[string]uint64
}

func newFSState(backend Backend) *fsState {
	return &fsState{
		backend:  backend,
		nextIno:  1,
		inoByKey: make(map[string]uint64),
	}
}

// fileStat refreshes only payload metadata; managed subtitles have their own
// independent snapshot/stat capabilities and must not pass through this path.
func (s *fsState) fileStat(hash metainfo.Hash, path string, size int64, createdAt time.Time) (FileStat, error) {
	stat := FileStat{Size: size, CreatedAt: createdAt}
	if backend, ok := s.backend.(FileStatBackend); ok {
		var err error
		stat, err = backend.FileStat(hash, path)
		if err != nil {
			return FileStat{}, err
		}
	}
	if stat.Size < 0 {
		return FileStat{}, ErrInvalidName
	}
	return stat, nil
}

// inoFor returns the stable inode number assigned to key, allocating a new
// one on first use.
func (s *fsState) inoFor(key string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ino, ok := s.inoByKey[key]; ok {
		return ino
	}
	s.nextIno++
	s.inoByKey[key] = s.nextIno
	return s.nextIno
}

// Mount mounts backend on mnt and returns the running server. Dynamic torrent
// views use zero kernel cache timeouts so namespace changes become visible
// without explicit invalidation calls.
func Mount(mnt string, backend Backend, opts *fs.Options) (*fuse.Server, error) {
	if opts == nil {
		opts = &fs.Options{}
	}
	// Peerless reads must not exhaust the mount-wide asynchronous read queue.
	opts.SyncRead = true
	zero := time.Duration(0)
	opts.EntryTimeout = &zero
	opts.AttrTimeout = &zero
	if opts.RootStableAttr == nil {
		opts.RootStableAttr = &fs.StableAttr{Ino: 1, Mode: fuse.S_IFDIR}
	}
	root := &rootNode{state: newFSState(backend)}
	return fs.Mount(mnt, root, opts)
}
