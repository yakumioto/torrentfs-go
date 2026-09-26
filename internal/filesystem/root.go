package filesystem

import (
	"context"
	"slices"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// rootNode is the mount root. Its children are dynamically read from the
// Backend and expose torrent data only.
type rootNode struct {
	fs.Inode
	state *fsState
}

func (n *rootNode) torrentViews() []TorrentView {
	return n.state.backend.Torrents()
}

func (n *rootNode) children() []rootEntry {
	entries := rootEntries(n.torrentViews())
	slices.SortFunc(entries, func(a, b rootEntry) int {
		return strings.Compare(a.Name, b.Name)
	})
	return entries
}

// visibleChildren returns the mount root's children: one entry per torrent,
// plus the managed subtitles of single-file torrents. A multi-file torrent's
// subtitles live inside its directory instead, so only root-level subtitle
// paths are lifted here.
func (n *rootNode) visibleChildren() []rootEntry {
	entries := n.children()
	used := make(map[string]bool, len(entries))
	for _, entry := range entries {
		used[entry.Name] = true
	}
	for _, entry := range entries {
		if _, single := mediaRoot(entry.View); !single {
			continue
		}
		for _, subtitle := range entry.View.Subtitles {
			if strings.Contains(subtitle.Path, "/") || used[subtitle.Path] {
				continue
			}
			used[subtitle.Path] = true
			entries = append(entries, rootEntry{
				Name:       subtitle.Path,
				View:       entry.View,
				Path:       subtitle.Path,
				Size:       subtitle.Size,
				ModifiedAt: subtitle.ModifiedAt,
				IsSubtitle: true,
			})
		}
	}
	slices.SortFunc(entries, func(a, b rootEntry) int {
		return strings.Compare(a.Name, b.Name)
	})
	return entries
}

func (n *rootNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o555
	// A directory link count of zero is not a valid POSIX stat result and makes
	// stat-driven consumers (for example smbd resolving share entries) reject
	// every child of the mount root.
	out.Nlink = 2
	return 0
}

func (n *rootNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	for _, c := range n.visibleChildren() {
		if c.Name != name {
			continue
		}
		if c.IsSubtitle {
			out.Mode = 0o444
			out.Size = uint64(c.Size)
			setModifiedAt(&out.Attr, c.ModifiedAt)
			child := &torrentFileNode{
				state:      n.state,
				hash:       c.View.Hash,
				path:       c.Path,
				size:       c.Size,
				subtitle:   true,
				modifiedAt: c.ModifiedAt,
			}
			return n.NewInode(ctx, child, fs.StableAttr{
				Mode: syscall.S_IFREG,
				Ino:  n.state.inoFor(fileKey(c.View.Hash, c.Path)),
			}), 0
		}
		if f, ok := mediaRoot(c.View); ok {
			out.Mode = 0o444
			out.Size = uint64(f.Size)
			setCreatedAt(&out.Attr, c.View.CreatedAt)
			child := &torrentFileNode{
				state:     n.state,
				hash:      c.View.Hash,
				path:      f.Path,
				size:      f.Size,
				createdAt: c.View.CreatedAt,
			}
			return n.NewInode(ctx, child, fs.StableAttr{
				Mode: syscall.S_IFREG,
				Ino:  n.state.inoFor(fileKey(c.View.Hash, f.Path)),
			}), 0
		}
		out.Mode = 0o555
		setCreatedAt(&out.Attr, c.View.CreatedAt)
		child := &torrentDirNode{
			state:     n.state,
			hash:      c.View.Hash,
			files:     c.View.Files,
			subtitles: c.View.Subtitles,
			createdAt: c.View.CreatedAt,
		}
		return n.NewInode(ctx, child, fs.StableAttr{
			Mode: syscall.S_IFDIR,
			Ino:  n.state.inoFor(torrentKey(c.View.Hash)),
		}), 0
	}
	return nil, errnoFor(ErrNotFound)
}

func (n *rootNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	cs := n.visibleChildren()
	entries := make([]fuse.DirEntry, 0, len(cs))
	for _, c := range cs {
		mode := uint32(syscall.S_IFDIR)
		if c.IsSubtitle || !c.isDir() {
			mode = syscall.S_IFREG
		}
		entries = append(entries, fuse.DirEntry{Name: c.Name, Mode: mode})
	}
	return fs.NewListDirStream(entries), 0
}

func (n *rootNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if _, ok := lookupRootEntry(n.visibleChildren(), name); ok {
		return nil, errnoFor(ErrExists)
	}
	return nil, errnoFor(ErrReadOnly)
}

func (n *rootNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	entry, ok := lookupRootEntry(n.visibleChildren(), name)
	if !ok {
		return errnoFor(ErrNotFound)
	}
	if !entry.isDir() {
		return errnoFor(ErrNotDir)
	}
	return errnoFor(ErrReadOnly)
}

func (n *rootNode) Unlink(ctx context.Context, name string) syscall.Errno {
	entry, ok := lookupRootEntry(n.visibleChildren(), name)
	if !ok {
		return errnoFor(ErrNotFound)
	}
	if entry.isDir() {
		return errnoFor(ErrIsDir)
	}
	return errnoFor(ErrReadOnly)
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
