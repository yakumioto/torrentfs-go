package filesystem

import (
	"context"
	"slices"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

const (
	metadataName  = "metadata"
	statsRootName = "stats"
)

// rootNode is the mount root. Its children are dynamically read from the
// Backend, plus the optional metadata control directory and the always-present
// read-only stats control directory.
type rootNode struct {
	fs.Inode
	state *fsState
}

func (n *rootNode) torrentViews() []TorrentView {
	return n.state.backend.Torrents()
}

func (n *rootNode) children() []rootEntry {
	entries := rootEntries(n.torrentViews())
	if n.state.metadata != nil && n.state.metadataExists() {
		entries = append(entries, rootEntry{Name: metadataName, Kind: rootMetadataKind})
	}
	entries = append(entries, rootEntry{Name: statsRootName, Kind: rootStatsKind})
	slices.SortFunc(entries, func(a, b rootEntry) int {
		return strings.Compare(a.Name, b.Name)
	})
	return entries
}

func (n *rootNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	mode := uint32(0o555)
	if n.state.metadata != nil {
		mode = 0o755
	}
	out.Mode = mode
	return 0
}

func (n *rootNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	for _, c := range n.children() {
		if c.Name != name {
			continue
		}
		switch {
		case c.Kind == rootMetadataKind:
			out.Mode = 0o755
			child := &metadataDirNode{state: n.state}
			return n.NewInode(ctx, child, fs.StableAttr{
				Mode: syscall.S_IFDIR,
				Ino:  n.state.inoFor(metadataKey()),
			}), 0
		case c.Kind == rootStatsKind:
			out.Mode = 0o555
			child := &statsRootNode{state: n.state}
			return n.NewInode(ctx, child, fs.StableAttr{
				Mode: syscall.S_IFDIR,
				Ino:  n.state.inoFor(statsKey()),
			}), 0
		}
		if f, ok := mediaRoot(c.View); ok {
			out.Mode = 0o444
			out.Size = uint64(f.Size)
			child := &torrentFileNode{state: n.state, hash: c.View.Hash, path: f.Path, size: f.Size}
			return n.NewInode(ctx, child, fs.StableAttr{
				Mode: syscall.S_IFREG,
				Ino:  n.state.inoFor(fileKey(c.View.Hash, f.Path)),
			}), 0
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
	return nil, errnoFor(ErrNotFound)
}

func (n *rootNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	cs := n.children()
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

func (n *rootNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if n.state.metadata == nil {
		return nil, errnoFor(ErrReadOnly)
	}
	if name != metadataName {
		if _, ok := lookupRootEntry(n.children(), name); ok {
			return nil, errnoFor(ErrExists)
		}
		return nil, errnoFor(ErrReadOnly)
	}
	if n.state.metadataExists() {
		return nil, errnoFor(ErrExists)
	}
	if err := n.state.metadata.EnsureMetadataDir(); err != nil {
		return nil, errnoFor(err)
	}
	n.state.setMetadataExists(true)
	out.Mode = 0o755
	child := &metadataDirNode{state: n.state}
	return n.NewInode(ctx, child, fs.StableAttr{
		Mode: syscall.S_IFDIR,
		Ino:  n.state.inoFor(metadataKey()),
	}), 0
}

func (n *rootNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	if name == metadataName {
		if n.state.metadata == nil {
			return errnoFor(ErrReadOnly)
		}
		if !n.state.metadataExists() {
			return errnoFor(ErrNotFound)
		}
		if err := n.state.metadata.RemoveMetadataDir(); err != nil {
			return errnoFor(err)
		}
		n.state.setMetadataExists(false)
		return 0
	}
	if entry, ok := lookupRootEntry(n.children(), name); ok {
		if !entry.isDir() {
			return errnoFor(ErrNotDir)
		}
		return errnoFor(ErrReadOnly)
	}
	return errnoFor(ErrNotFound)
}

func (n *rootNode) Unlink(ctx context.Context, name string) syscall.Errno {
	if name == metadataName {
		return errnoFor(ErrIsDir)
	}
	if _, ok := lookupRootEntry(n.children(), name); ok {
		return errnoFor(ErrReadOnly)
	}
	return errnoFor(ErrNotFound)
}

func (n *rootNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	return errnoFor(ErrReadOnly)
}

func (n *rootNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	return nil, nil, 0, errnoFor(ErrReadOnly)
}

func lookupRootEntry(entries []rootEntry, name string) (rootEntry, bool) {
	for _, entry := range entries {
		if entry.Name == name {
			return entry, true
		}
	}
	return rootEntry{}, false
}

var (
	_ fs.NodeGetattrer = (*rootNode)(nil)
	_ fs.NodeLookuper  = (*rootNode)(nil)
	_ fs.NodeReaddirer = (*rootNode)(nil)
	_ fs.NodeMkdirer   = (*rootNode)(nil)
	_ fs.NodeRmdirer   = (*rootNode)(nil)
	_ fs.NodeUnlinker  = (*rootNode)(nil)
	_ fs.NodeRenamer   = (*rootNode)(nil)
	_ fs.NodeCreater   = (*rootNode)(nil)
)
