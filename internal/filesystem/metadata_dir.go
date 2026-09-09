package filesystem

import (
	"context"
	"slices"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type metadataDirNode struct {
	fs.Inode
	state *fsState
}

func (n *metadataDirNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o755
	return 0
}

func (n *metadataDirNode) views() []MetadataView {
	views := n.state.metadata.MetadataFiles()
	slices.SortFunc(views, func(a, b MetadataView) int {
		return strings.Compare(a.Name, b.Name)
	})
	return views
}

func (n *metadataDirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if !validMetadataName(name) {
		return nil, errnoFor(ErrInvalidName)
	}
	for _, view := range n.views() {
		if view.Name != name {
			continue
		}
		out.Mode = 0o644
		out.Size = uint64(view.Size)
		child := &metadataFileNode{state: n.state, name: name, size: view.Size}
		n.state.rememberMetadataNode(name, child)
		return n.NewInode(ctx, child, fs.StableAttr{
			Mode: syscall.S_IFREG,
			Ino:  n.state.inoFor(metadataFileKey(name)),
		}), 0
	}
	return nil, errnoFor(ErrNotFound)
}

func (n *metadataDirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	views := n.views()
	entries := make([]fuse.DirEntry, 0, len(views))
	for _, view := range views {
		entries = append(entries, fuse.DirEntry{Name: view.Name, Mode: syscall.S_IFREG})
	}
	return fs.NewListDirStream(entries), 0
}

func (n *metadataDirNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	if n.state.metadata == nil {
		return nil, nil, 0, errnoFor(ErrReadOnly)
	}
	if !validMetadataName(name) {
		return nil, nil, 0, errnoFor(ErrInvalidName)
	}
	writer, err := n.state.metadata.BeginMetadata(ctx, name, flags)
	if err != nil {
		return nil, nil, 0, errnoFor(err)
	}
	child := &metadataFileNode{state: n.state, name: name}
	n.state.rememberMetadataNode(name, child)
	handle := &metadataWriteHandle{writer: writer, node: child}
	out.Mode = 0o644
	return n.NewInode(ctx, child, fs.StableAttr{
		Mode: syscall.S_IFREG,
		Ino:  n.state.inoFor(metadataFileKey(name)),
	}), handle, 0, 0
}

func (n *metadataDirNode) Unlink(ctx context.Context, name string) syscall.Errno {
	if n.state.metadata == nil {
		return errnoFor(ErrReadOnly)
	}
	if !validMetadataName(name) {
		return errnoFor(ErrInvalidName)
	}
	err := n.state.metadata.RemoveMetadata(ctx, name)
	if err == nil {
		n.state.forgetMetadataNode(name)
	}
	return errnoFor(err)
}

func (n *metadataDirNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	if n.state.metadata == nil {
		return errnoFor(ErrReadOnly)
	}
	if flags != 0 || !validMetadataName(name) || !validMetadataName(newName) || name == newName {
		return errnoFor(ErrInvalidName)
	}
	destination, ok := newParent.(*metadataDirNode)
	if !ok || destination.state != n.state {
		return errnoFor(ErrCrossDir)
	}
	err := n.state.metadata.RenameMetadata(ctx, name, newName)
	if err == nil {
		n.state.renameMetadataNode(name, newName)
	}
	return errnoFor(err)
}

func (n *metadataDirNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	if !validMetadataName(name) {
		return errnoFor(ErrInvalidName)
	}
	for _, view := range n.views() {
		if view.Name == name {
			return errnoFor(ErrNotDir)
		}
	}
	return errnoFor(ErrNotFound)
}

func (n *metadataDirNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, errnoFor(ErrReadOnly)
}

func validMetadataName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, "/\\\x00") &&
		strings.HasSuffix(name, ".torrent")
}

func metadataFileKey(name string) string { return "m/" + name }

var (
	_ fs.NodeGetattrer = (*metadataDirNode)(nil)
	_ fs.NodeLookuper  = (*metadataDirNode)(nil)
	_ fs.NodeReaddirer = (*metadataDirNode)(nil)
	_ fs.NodeCreater   = (*metadataDirNode)(nil)
	_ fs.NodeUnlinker  = (*metadataDirNode)(nil)
	_ fs.NodeRenamer   = (*metadataDirNode)(nil)
	_ fs.NodeRmdirer   = (*metadataDirNode)(nil)
	_ fs.NodeMkdirer   = (*metadataDirNode)(nil)
)
