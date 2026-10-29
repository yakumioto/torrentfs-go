// Package filesystem adapts the shared read-only virtual tree to FUSE.
package filesystem

import (
	"sync"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/yakumioto/torrentfs-go/internal/vfs"
)

// Compatibility aliases keep existing Backend implementations source-compatible.
// The model is owned by vfs and has no dependency on FUSE.
type FileView = vfs.FileView
type SubtitleView = vfs.SubtitleView
type SubtitleStat = vfs.SubtitleStat
type FileListID = vfs.FileListID
type TorrentContents = vfs.TorrentContents
type TorrentContentsBackend = vfs.TorrentContentsBackend
type TorrentView = vfs.TorrentView
type Backend = vfs.Backend
type FileStat = vfs.FileStat
type FileSnapshot = vfs.FileSnapshot
type FileStatBackend = vfs.FileStatBackend
type FileSnapshotBackend = vfs.FileSnapshotBackend
type CategoryBackend = vfs.CategoryBackend
type SubtitleSnapshot = vfs.SubtitleSnapshot
type SubtitleBackend = vfs.SubtitleBackend
type SubtitleStatBackend = vfs.SubtitleStatBackend

func setCreatedAt(attr *fuse.Attr, createdAt time.Time) {
	if !createdAt.IsZero() {
		attr.SetTimes(&createdAt, &createdAt, &createdAt)
	}
}

func setModifiedAt(attr *fuse.Attr, modifiedAt time.Time) {
	if !modifiedAt.IsZero() {
		attr.SetTimes(&modifiedAt, &modifiedAt, &modifiedAt)
	}
}

// fsState keeps FUSE inode allocation and its membership indexes mount-private.
type fsState struct {
	backend         Backend
	views           snapshotCache[torrentDirectoryVersion]
	directoryBuilds uint64 // guarded by views.mu
	mu              sync.Mutex
	nextIno         uint64
	inoByKey        map[string]uint64
}

func newFSState(backend Backend) *fsState {
	return &fsState{backend: backend, nextIno: 1, inoByKey: make(map[string]uint64)}
}

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

func MountTree(mnt string, tree *vfs.Tree, opts *fs.Options) (*fuse.Server, error) {
	return Mount(mnt, tree.Backend(), opts)
}

// Mount uses zero entry/attribute timeouts so namespace changes stay visible.
func Mount(mnt string, backend Backend, opts *fs.Options) (*fuse.Server, error) {
	if opts == nil {
		opts = &fs.Options{}
	}
	opts.SyncRead = true
	zero := time.Duration(0)
	opts.EntryTimeout, opts.AttrTimeout = &zero, &zero
	if opts.RootStableAttr == nil {
		opts.RootStableAttr = &fs.StableAttr{Ino: 1, Mode: fuse.S_IFDIR}
	}
	return fs.Mount(mnt, &rootNode{state: newFSState(backend)}, opts)
}
