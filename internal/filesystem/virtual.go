package filesystem

import (
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

// VirtualEntry describes a protocol-independent visible file or directory.
type VirtualEntry struct {
	Name       string
	Size       int64
	IsDir      bool
	CreatedAt  time.Time
	ModifiedAt time.Time
}

// VirtualFS resolves the same live namespace as FUSE without mounting it.
type VirtualFS struct {
	state *fsState
}

func NewVirtualFS(backend Backend) *VirtualFS {
	return &VirtualFS{state: newFSState(backend)}
}

type virtualNode struct {
	VirtualEntry
	hash     metainfo.Hash
	path     string
	subtitle bool
	category string
	group    bool
}

func (v *VirtualFS) groupEntries(category string) []rootEntry {
	views := v.state.torrentViews()
	var directories []rootEntry
	var reserved map[string]bool
	if category == "" {
		if backend, ok := v.state.backend.(CategoryBackend); ok {
			categories := backend.Categories()
			reserved = categoryReservations(categories)
			for _, name := range categories {
				directories = append(directories, rootEntry{Name: name, IsCategory: true})
			}
		}
	}
	return visibleEntries(rootEntriesForCategory(views, category, reserved), directories)
}

func rootVirtualNode(e rootEntry) virtualNode {
	n := virtualNode{
		VirtualEntry: VirtualEntry{Name: e.Name, IsDir: e.isDir(), CreatedAt: e.View.CreatedAt, ModifiedAt: e.ModifiedAt},
		hash:         e.View.Hash, path: e.Path, subtitle: e.IsSubtitle,
	}
	switch {
	case e.IsCategory:
		n.category, n.group = e.Name, true
	case e.IsSubtitle:
		n.IsDir, n.Size = false, e.Size
	case !n.IsDir:
		file, _ := mediaRoot(e.View)
		n.path, n.Size = file.Path, file.Size
	}
	return n
}

func childVirtualNode(hash metainfo.Hash, createdAt time.Time, prefix string, e fsEntry) virtualNode {
	p := e.Path
	if e.IsDir {
		p = joinRel(prefix, e.Name)
	}
	return virtualNode{
		VirtualEntry: VirtualEntry{Name: e.Name, Size: e.Size, IsDir: e.IsDir, CreatedAt: createdAt, ModifiedAt: e.ModifiedAt},
		hash:         hash, path: p, subtitle: e.IsSubtitle,
	}
}

func (v *VirtualFS) resolve(name string) (virtualNode, error) {
	if name == "" || name == "." {
		return virtualNode{VirtualEntry: VirtualEntry{IsDir: true}, group: true}, nil
	}
	if !fs.ValidPath(name) || strings.ContainsAny(name, "\\\x00") {
		return virtualNode{}, fs.ErrInvalid
	}
	parts := strings.Split(name, "/")
	n := virtualNode{VirtualEntry: VirtualEntry{IsDir: true}, group: true}
	for _, part := range parts {
		if !n.IsDir {
			return virtualNode{}, ErrNotDir
		}
		if n.group {
			entry, ok := lookupRootEntry(v.groupEntries(n.category), part)
			if !ok {
				return virtualNode{}, ErrNotFound
			}
			n = rootVirtualNode(entry)
			continue
		}
		contents, index, err := v.state.directory(n.hash, n.path)
		if err != nil {
			return virtualNode{}, err
		}
		entry, ok := index.byName[part]
		if !ok {
			return virtualNode{}, ErrNotFound
		}
		n = childVirtualNode(n.hash, contents.CreatedAt, n.path, entry)
	}
	return n, nil
}

func (v *VirtualFS) Stat(name string) (VirtualEntry, error) {
	n, err := v.resolve(name)
	if err != nil {
		return VirtualEntry{}, err
	}
	if n.IsDir {
		return n.VirtualEntry, nil
	}
	if n.subtitle {
		if backend, ok := v.state.backend.(SubtitleStatBackend); ok {
			stat, err := backend.SubtitleStat(n.hash, n.path)
			if err != nil {
				return VirtualEntry{}, err
			}
			n.Size, n.ModifiedAt = stat.Size, stat.ModifiedAt
		}
	} else {
		stat, err := v.state.fileStat(n.hash, n.path, n.Size, n.CreatedAt)
		if err != nil {
			return VirtualEntry{}, err
		}
		n.Size, n.CreatedAt = stat.Size, stat.CreatedAt
	}
	return n.VirtualEntry, nil
}

func (v *VirtualFS) ReadDir(name string) ([]VirtualEntry, error) {
	n, err := v.resolve(name)
	if err != nil {
		return nil, err
	}
	if !n.IsDir {
		return nil, ErrNotDir
	}
	var entries []VirtualEntry
	if n.group {
		for _, e := range v.groupEntries(n.category) {
			entries = append(entries, rootVirtualNode(e).VirtualEntry)
		}
		return entries, nil
	}
	contents, index, err := v.state.directory(n.hash, n.path)
	if err != nil {
		return nil, err
	}
	for _, e := range index.entries {
		entries = append(entries, childVirtualNode(n.hash, contents.CreatedAt, n.path, e).VirtualEntry)
	}
	return entries, nil
}

// Open returns a reader and metadata from the same open; directories have no reader.
func (v *VirtualFS) Open(name string) (io.ReaderAt, VirtualEntry, error) {
	n, err := v.resolve(name)
	if err != nil {
		return nil, VirtualEntry{}, err
	}
	if n.IsDir {
		return nil, n.VirtualEntry, nil
	}
	var reader io.ReaderAt
	if n.subtitle {
		backend, ok := v.state.backend.(SubtitleBackend)
		if !ok {
			return nil, VirtualEntry{}, ErrNotFound
		}
		snapshot, openErr := backend.OpenSubtitle(n.hash, n.path)
		if openErr != nil {
			return nil, VirtualEntry{}, openErr
		}
		reader, n.Size, n.ModifiedAt = snapshot.Reader, snapshot.Size, snapshot.ModifiedAt
	} else if backend, ok := v.state.backend.(FileSnapshotBackend); ok {
		snapshot, openErr := backend.OpenFileSnapshot(n.hash, n.path)
		if openErr != nil {
			return nil, VirtualEntry{}, openErr
		}
		reader, n.Size, n.CreatedAt = snapshot.Reader, snapshot.Size, snapshot.CreatedAt
	} else {
		reader, err = v.state.backend.OpenFile(n.hash, n.path)
		if err != nil {
			return nil, VirtualEntry{}, err
		}
	}
	if reader == nil || n.Size < 0 {
		if closer, ok := reader.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, VirtualEntry{}, ErrInvalidName
	}
	return reader, n.VirtualEntry, nil
}
