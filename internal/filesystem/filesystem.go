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

// TorrentView is the read-only snapshot of a torrent exposed to the
// filesystem layer. Files is non-empty once the torrent's metainfo is known.
// SingleFile marks a torrent whose metainfo has no directory structure
// (metainfo.Info.IsDir() is false): its one file is exposed directly as a
// regular file at the mount root instead of inside a directory. The zero
// value keeps the directory layout, so a Backend that never sets it still gets
// the historical behaviour.
type TorrentView struct {
	Name       string
	Hash       metainfo.Hash
	Files      []FileView
	SingleFile bool
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

func hasMountOption(opts *fs.Options, want string) bool {
	for _, o := range opts.MountOptions.Options {
		if o == want {
			return true
		}
	}
	return false
}
