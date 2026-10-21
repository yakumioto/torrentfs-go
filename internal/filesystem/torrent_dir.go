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
	prefix string // torrent-relative directory prefix, resolved on every operation
}

func (n *torrentDirNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	contents, _, err := n.state.directory(n.hash, n.prefix)
	if err != nil {
		return errnoFor(err)
	}
	out.Mode = 0o555
	out.Nlink = 2
	setCreatedAt(&out.Attr, contents.CreatedAt)
	return 0
}

func (n *torrentDirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	contents, index, err := n.state.directory(n.hash, n.prefix)
	if err != nil {
		return nil, errnoFor(err)
	}
	e, ok := index.byName[name]
	if !ok {
		return nil, errnoFor(ErrNotFound)
	}
	if e.IsDir {
		out.Mode = 0o555
		setCreatedAt(&out.Attr, contents.CreatedAt)
		prefix := joinRel(n.prefix, e.Name)
		child := &torrentDirNode{state: n.state, hash: n.hash, prefix: prefix}
		return n.NewInode(ctx, child, fs.StableAttr{
			Mode: syscall.S_IFDIR,
			Ino:  n.state.inoFor(dirKey(n.hash, prefix)),
		}), 0
	}
	createdAt := contents.CreatedAt
	if !e.IsSubtitle {
		stat, err := n.state.fileStat(n.hash, e.Path, e.Size, createdAt)
		if err != nil {
			return nil, errnoFor(err)
		}
		e.Size, createdAt = stat.Size, stat.CreatedAt
	}
	out.Mode = 0o444
	out.Size = uint64(e.Size)
	if e.IsSubtitle {
		setModifiedAt(&out.Attr, e.ModifiedAt)
	} else {
		setCreatedAt(&out.Attr, createdAt)
	}
	child := &torrentFileNode{
		state:      n.state,
		hash:       n.hash,
		path:       e.Path,
		size:       e.Size,
		subtitle:   e.IsSubtitle,
		createdAt:  createdAt,
		modifiedAt: e.ModifiedAt,
	}
	return n.NewInode(ctx, child, fs.StableAttr{
		Mode: syscall.S_IFREG,
		Ino:  n.state.inoFor(fileKey(n.hash, e.Path)),
	}), 0
}

func (n *torrentDirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	_, index, err := n.state.directory(n.hash, n.prefix)
	if err != nil {
		return nil, errnoFor(err)
	}
	entries := make([]fuse.DirEntry, 0, len(index.entries))
	for _, c := range index.entries {
		mode := uint32(syscall.S_IFREG)
		if c.IsDir {
			mode = syscall.S_IFDIR
		}
		entries = append(entries, fuse.DirEntry{Name: c.Name, Mode: mode})
	}
	return fs.NewListDirStream(entries), 0
}

func (n *torrentDirNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, errnoFor(ErrReadOnly)
}

func (n *torrentDirNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	return nil, nil, 0, errnoFor(ErrReadOnly)
}

func (n *torrentDirNode) Unlink(ctx context.Context, name string) syscall.Errno {
	return errnoFor(ErrReadOnly)
}

func (n *torrentDirNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	return errnoFor(ErrReadOnly)
}

func (n *torrentDirNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	return errnoFor(ErrReadOnly)
}

var (
	_ fs.NodeMkdirer  = (*torrentDirNode)(nil)
	_ fs.NodeCreater  = (*torrentDirNode)(nil)
	_ fs.NodeUnlinker = (*torrentDirNode)(nil)
	_ fs.NodeRmdirer  = (*torrentDirNode)(nil)
	_ fs.NodeRenamer  = (*torrentDirNode)(nil)
)
