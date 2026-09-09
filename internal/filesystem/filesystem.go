// Package filesystem adapts torrent session data to a FUSE filesystem. It
// contains no network or download logic of its own: every piece of torrent
// data it serves comes through the Backend interface it declares, which the
// session implements and main injects.
package filesystem

import (
	"context"
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

// MetadataView describes one metadata file in the control directory.
type MetadataView struct {
	Name string
	Size int64
}

// MetadataReader is a per-open metadata file handle.
type MetadataReader interface {
	io.ReaderAt
	io.Closer
}

// MetadataWriter receives an atomically committed metadata file.
type MetadataWriter interface {
	io.WriterAt
	Commit() error
	Abort() error
}

// MetadataBackend supplies the optional metadata control directory. It is
// deliberately narrower than the session implementation so read-only fakes
// only need to implement Backend.
type MetadataBackend interface {
	Backend
	MetadataFiles() []MetadataView
	OpenMetadata(name string) (MetadataReader, error)
	BeginMetadata(ctx context.Context, name string, flags uint32) (MetadataWriter, error)
	RemoveMetadata(ctx context.Context, name string) error
	RenameMetadata(ctx context.Context, oldName, newName string) error
	EnsureMetadataDir() error
	RemoveMetadataDir() error
}

// metadataDirState is an optional extension used to distinguish an empty
// metadata directory from one removed through the filesystem.
type metadataDirState interface {
	MetadataDirExists() bool
}

// fsState carries the pieces shared by every node in a mount: the Backend,
// optional metadata support, and the stable inode allocator.
type fsState struct {
	backend  Backend
	metadata MetadataBackend

	mu              sync.Mutex
	nextIno         uint64
	inoByKey        map[string]uint64
	metadataNodes   map[string]*metadataFileNode
	metadataPresent bool
}

func newFSState(backend Backend) *fsState {
	metadata, _ := backend.(MetadataBackend)
	metadataPresent := metadata != nil
	if checker, ok := backend.(metadataDirState); ok {
		metadataPresent = checker.MetadataDirExists()
	}
	return &fsState{
		backend:         backend,
		metadata:        metadata,
		nextIno:         1, // root takes inode 1; children start at 2
		inoByKey:        make(map[string]uint64),
		metadataNodes:   make(map[string]*metadataFileNode),
		metadataPresent: metadataPresent,
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

func (s *fsState) metadataExists() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metadataPresent
}

func (s *fsState) setMetadataExists(present bool) {
	s.mu.Lock()
	s.metadataPresent = present
	s.mu.Unlock()
}

func (s *fsState) rememberMetadataNode(name string, node *metadataFileNode) {
	s.mu.Lock()
	if s.metadataNodes == nil {
		s.metadataNodes = make(map[string]*metadataFileNode)
	}
	s.metadataNodes[name] = node
	s.mu.Unlock()
}

func (s *fsState) forgetMetadataNode(name string) {
	s.mu.Lock()
	delete(s.metadataNodes, name)
	s.mu.Unlock()
}

func (s *fsState) renameMetadataNode(oldName, newName string) {
	s.mu.Lock()
	if node := s.metadataNodes[oldName]; node != nil {
		delete(s.metadataNodes, oldName)
		s.metadataNodes[newName] = node
		node.mu.Lock()
		node.name = newName
		node.mu.Unlock()
	}
	if ino := s.inoByKey[metadataFileKey(oldName)]; ino != 0 {
		s.inoByKey[metadataFileKey(newName)] = ino
	}
	s.mu.Unlock()
}

// Mount mounts backend on mnt and returns the running server. Dynamic
// metadata and torrent views use zero kernel cache timeouts so namespace
// changes become visible without explicit invalidation calls.
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
