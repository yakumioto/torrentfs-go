// Package filesystem adapts torrent session data to a read-only FUSE
// filesystem. It contains no network or download logic of its own: every
// piece of torrent data it serves comes through the Backend interface it
// declares, which the session implements and main injects.
package filesystem

import (
	"io"
	"sync"

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
type TorrentView struct {
	Name  string
	Hash  metainfo.Hash
	Files []FileView
}

// Backend supplies the filesystem layer with torrent snapshots and file
// handles.
type Backend interface {
	// Torrents returns the torrents to expose. Only torrents whose metainfo
	// is available are included.
	Torrents() []TorrentView
	// OpenFile returns a handle for reading the file at the given display
	// path inside the torrent identified by hash.
	OpenFile(hash metainfo.Hash, path string) (io.ReaderAt, error)
}

// fsState carries the pieces shared by every node in a mount: the Backend
// and the stable inode allocator. Inode numbers are assigned once per child
// identity and reused on every later lookup, so go-fuse can recognise the
// same object across calls and keep its identity stable.
type fsState struct {
	backend  Backend
	mu       sync.Mutex
	nextIno  uint64
	inoByKey map[string]uint64
}

func newFSState(backend Backend) *fsState {
	return &fsState{
		backend:  backend,
		nextIno:  1, // root takes inode 1; children start at 2
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

// Mount mounts backend on mnt as a read-only FUSE filesystem and returns the
// running server. opts is used as-is except that a missing "ro" option is
// appended, and a missing RootStableAttr is set to inode 1.
func Mount(mnt string, backend Backend, opts *fs.Options) (*fuse.Server, error) {
	if opts == nil {
		opts = &fs.Options{}
	}
	if !hasMountOption(opts, "ro") {
		opts.MountOptions.Options = append(opts.MountOptions.Options, "ro")
	}
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
