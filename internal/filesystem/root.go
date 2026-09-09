package filesystem

import (
	"context"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// rootNode is the mount root. Its children are one read-only directory per
// torrent, snapshotted from the Backend on first access.
type rootNode struct {
	fs.Inode
	state *fsState

	mu     sync.Mutex
	views  []TorrentView // nil until first access
	loaded bool
}

func (n *rootNode) torrentViews() []TorrentView {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.loaded {
		n.views = n.state.backend.Torrents()
		n.loaded = true
	}
	return n.views
}

func (n *rootNode) children() []rootEntry {
	return rootEntries(n.torrentViews())
}

func (n *rootNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o555
	return 0
}

func (n *rootNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	for _, c := range n.children() {
		if c.Name != name {
			continue
		}
		out.Mode = 0o555
		child := &torrentDirNode{
			state: n.state,
			hash:  c.View.Hash,
			files: c.View.Files,
		}
		return n.NewInode(ctx, child, fs.StableAttr{
			Mode: syscall.S_IFDIR,
			Ino:  n.state.inoFor(torrentKey(c.View.Hash)),
		}), 0
	}
	return nil, syscall.ENOENT
}

func (n *rootNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	cs := n.children()
	entries := make([]fuse.DirEntry, 0, len(cs))
	for _, c := range cs {
		entries = append(entries, fuse.DirEntry{Name: c.Name, Mode: syscall.S_IFDIR})
	}
	return fs.NewListDirStream(entries), 0
}
