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
	// CreatedAt is the durable time when the torrent was added to the session.
	CreatedAt  time.Time
	Files      []FileView
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
	zero := time.Duration(0)
	opts.EntryTimeout = &zero
	opts.AttrTimeout = &zero
	if opts.RootStableAttr == nil {
		opts.RootStableAttr = &fs.StableAttr{Ino: 1, Mode: fuse.S_IFDIR}
	}
	root := &rootNode{state: newFSState(backend)}
	return fs.Mount(mnt, root, opts)
}
