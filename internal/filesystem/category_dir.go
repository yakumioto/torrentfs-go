package filesystem

import (
	"context"
	"slices"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// categoryNode is a dynamic, read-only directory containing one category's
// torrents. Its children use the same torrent layout as the mount root.
type categoryNode struct {
	fs.Inode
	state    *fsState
	category string
}

func (n *categoryNode) torrentViews() []TorrentView {
	views := n.state.backend.Torrents()
	filtered := make([]TorrentView, 0, len(views))
	for _, view := range views {
		if view.Category == n.category {
			filtered = append(filtered, view)
		}
	}
	return filtered
}

func (n *categoryNode) children() []rootEntry {
	return rootEntriesForCategory(n.torrentViews(), n.category, nil)
}

func (n *categoryNode) visibleChildren() []rootEntry {
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

func (n *categoryNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o555
	out.Nlink = 2
	return 0
}

func (n *categoryNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
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

func (n *categoryNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	entries := n.visibleChildren()
	out := make([]fuse.DirEntry, 0, len(entries))
	for _, entry := range entries {
		mode := uint32(syscall.S_IFDIR)
		if entry.IsSubtitle || !entry.isDir() {
			mode = syscall.S_IFREG
		}
		out = append(out, fuse.DirEntry{Name: entry.Name, Mode: mode})
	}
	return fs.NewListDirStream(out), 0
}

func (n *categoryNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, errnoFor(ErrReadOnly)
}

func (n *categoryNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	return nil, nil, 0, errnoFor(ErrReadOnly)
}

func (n *categoryNode) Unlink(ctx context.Context, name string) syscall.Errno {
	return errnoFor(ErrReadOnly)
}

func (n *categoryNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	return errnoFor(ErrReadOnly)
}

func (n *categoryNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	return errnoFor(ErrReadOnly)
}

var (
	_ fs.NodeGetattrer = (*categoryNode)(nil)
	_ fs.NodeLookuper  = (*categoryNode)(nil)
	_ fs.NodeReaddirer = (*categoryNode)(nil)
	_ fs.NodeMkdirer   = (*categoryNode)(nil)
	_ fs.NodeCreater   = (*categoryNode)(nil)
	_ fs.NodeUnlinker  = (*categoryNode)(nil)
	_ fs.NodeRmdirer   = (*categoryNode)(nil)
	_ fs.NodeRenamer   = (*categoryNode)(nil)
)
