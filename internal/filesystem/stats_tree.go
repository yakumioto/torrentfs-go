package filesystem

import (
	"context"
	"syscall"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// statsRootNode is the read-only stats/ control directory. Its children mirror
// the mount's torrent roots with the same disambiguated names the data tree
// uses: a single-file torrent becomes one status file, a multi-file torrent a
// status directory tree. Every write operation is refused; the tree exists
// only to report piece state.
type statsRootNode struct {
	fs.Inode
	state *fsState
}

func (n *statsRootNode) entries() []rootEntry {
	return rootEntries(n.state.backend.Torrents())
}

func (n *statsRootNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o555
	return 0
}

func (n *statsRootNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	for _, c := range n.entries() {
		if c.Name != name {
			continue
		}
		if f, ok := mediaRoot(c.View); ok {
			out.Mode = 0o444
			child := &statsFileNode{state: n.state, hash: c.View.Hash, path: f.Path}
			content, err := child.snapshot()
			if err != nil {
				return nil, errnoFor(err)
			}
			out.Size = uint64(len(content))
			return n.NewInode(ctx, child, fs.StableAttr{
				Mode: syscall.S_IFREG,
				Ino:  n.state.inoFor(statsFileKey(c.View.Hash, f.Path)),
			}), 0
		}
		out.Mode = 0o555
		child := &statsDirNode{state: n.state, hash: c.View.Hash, files: c.View.Files}
		return n.NewInode(ctx, child, fs.StableAttr{
			Mode: syscall.S_IFDIR,
			Ino:  n.state.inoFor(statsDirKey(c.View.Hash, "")),
		}), 0
	}
	return nil, errnoFor(ErrNotFound)
}

func (n *statsRootNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	cs := n.entries()
	entries := make([]fuse.DirEntry, 0, len(cs))
	for _, c := range cs {
		mode := uint32(syscall.S_IFDIR)
		if !c.isDir() {
			mode = syscall.S_IFREG
		}
		entries = append(entries, fuse.DirEntry{Name: c.Name, Mode: mode})
	}
	return fs.NewListDirStream(entries), 0
}

func (n *statsRootNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, errnoFor(ErrReadOnly)
}

func (n *statsRootNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	return nil, nil, 0, errnoFor(ErrReadOnly)
}

func (n *statsRootNode) Unlink(ctx context.Context, name string) syscall.Errno {
	return errnoFor(ErrReadOnly)
}

func (n *statsRootNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	return errnoFor(ErrReadOnly)
}

func (n *statsRootNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	return errnoFor(ErrReadOnly)
}

// statsDirNode mirrors one directory of a multi-file torrent inside stats/.
// Its entries come from the same childrenOf the data tree uses, so the status
// tree and the data tree always have the same shape.
type statsDirNode struct {
	fs.Inode
	state  *fsState
	hash   metainfo.Hash
	files  []FileView
	prefix string
}

func (n *statsDirNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o555
	return 0
}

func (n *statsDirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	e, ok := lookupChild(n.files, n.prefix, name)
	if !ok {
		return nil, errnoFor(ErrNotFound)
	}
	if e.IsDir {
		out.Mode = 0o555
		prefix := joinRel(n.prefix, e.Name)
		child := &statsDirNode{state: n.state, hash: n.hash, files: n.files, prefix: prefix}
		return n.NewInode(ctx, child, fs.StableAttr{
			Mode: syscall.S_IFDIR,
			Ino:  n.state.inoFor(statsDirKey(n.hash, prefix)),
		}), 0
	}
	out.Mode = 0o444
	child := &statsFileNode{state: n.state, hash: n.hash, path: e.Path}
	content, err := child.snapshot()
	if err != nil {
		return nil, errnoFor(err)
	}
	out.Size = uint64(len(content))
	return n.NewInode(ctx, child, fs.StableAttr{
		Mode: syscall.S_IFREG,
		Ino:  n.state.inoFor(statsFileKey(n.hash, e.Path)),
	}), 0
}

func (n *statsDirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
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

func (n *statsDirNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, errnoFor(ErrReadOnly)
}

func (n *statsDirNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	return nil, nil, 0, errnoFor(ErrReadOnly)
}

func (n *statsDirNode) Unlink(ctx context.Context, name string) syscall.Errno {
	return errnoFor(ErrReadOnly)
}

func (n *statsDirNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	return errnoFor(ErrReadOnly)
}

func (n *statsDirNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	return errnoFor(ErrReadOnly)
}

var (
	_ fs.NodeGetattrer = (*statsRootNode)(nil)
	_ fs.NodeLookuper  = (*statsRootNode)(nil)
	_ fs.NodeReaddirer = (*statsRootNode)(nil)
	_ fs.NodeMkdirer   = (*statsRootNode)(nil)
	_ fs.NodeCreater   = (*statsRootNode)(nil)
	_ fs.NodeUnlinker  = (*statsRootNode)(nil)
	_ fs.NodeRmdirer   = (*statsRootNode)(nil)
	_ fs.NodeRenamer   = (*statsRootNode)(nil)
	_ fs.NodeGetattrer = (*statsDirNode)(nil)
	_ fs.NodeLookuper  = (*statsDirNode)(nil)
	_ fs.NodeReaddirer = (*statsDirNode)(nil)
	_ fs.NodeMkdirer   = (*statsDirNode)(nil)
	_ fs.NodeCreater   = (*statsDirNode)(nil)
	_ fs.NodeUnlinker  = (*statsDirNode)(nil)
	_ fs.NodeRmdirer   = (*statsDirNode)(nil)
	_ fs.NodeRenamer   = (*statsDirNode)(nil)
)
