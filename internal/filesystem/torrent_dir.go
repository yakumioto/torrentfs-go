package filesystem

import (
	"context"
	"syscall"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// torrentDirNode is a directory inside a torrent: its top directory (relPrefix
// "") or one of its virtual subdirectories. All of them flatten the torrent's
// files below the node's prefix; the same type recurses so any depth of file
// layout is served.
type torrentDirNode struct {
	fs.Inode
	state  *fsState
	hash   metainfo.Hash
	files  []FileView // display paths of every file in the torrent
	prefix string     // torrent-relative prefix this directory represents
}

func (n *torrentDirNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o555
	return 0
}

func (n *torrentDirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	e, ok := lookupChild(n.files, n.prefix, name)
	if !ok {
		return nil, syscall.ENOENT
	}
	if e.IsDir {
		out.Mode = 0o555
		prefix := joinRel(n.prefix, e.Name)
		child := &torrentDirNode{state: n.state, hash: n.hash, files: n.files, prefix: prefix}
		return n.NewInode(ctx, child, fs.StableAttr{
			Mode: syscall.S_IFDIR,
			Ino:  n.state.inoFor(dirKey(n.hash, prefix)),
		}), 0
	}
	out.Mode = 0o444
	out.Size = uint64(e.Size)
	child := &torrentFileNode{state: n.state, hash: n.hash, path: e.Path, size: e.Size}
	return n.NewInode(ctx, child, fs.StableAttr{
		Mode: syscall.S_IFREG,
		Ino:  n.state.inoFor(fileKey(n.hash, e.Path)),
	}), 0
}

func (n *torrentDirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	cs := childrenOf(n.files, n.prefix)
	entries := make([]fuse.DirEntry, 0, len(cs))
	for _, c := range cs {
		mode := uint32(syscall.S_IFREG)
		if c.IsDir {
			mode = syscall.S_IFDIR
		}
		entries = append(entries, fuse.DirEntry{Name: c.Name, Mode: mode})
	}
	return fs.NewListDirStream(entries), 0
}
