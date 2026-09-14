package filesystem

import (
	"fmt"
	"slices"
	"strings"

	"github.com/anacrolix/torrent/metainfo"
)

// fsEntry is one immediate child of a torrent directory: a leaf file or a
// (possibly virtual) subdirectory holding deeper files.
type fsEntry struct {
	Name  string
	IsDir bool
	// Path is the torrent-relative display path of the leaf file; set only
	// for file entries.
	Path string
	// Size is the leaf file's length; set only for file entries.
	Size int64
}

// childrenOf returns the sorted immediate children of the torrent directory
// at relPrefix ("" for the torrent's top directory), derived from the file
// display paths of one torrent. It is a pure function: the mount nodes only
// wrap its result for Lookup and Readdir.
func childrenOf(files []FileView, relPrefix string) []fsEntry {
	prefix := relPrefix
	if prefix != "" {
		prefix += "/"
	}
	seen := make(map[string]*fsEntry)
	for _, f := range files {
		rest, ok := strings.CutPrefix(f.Path, prefix)
		if !ok || rest == "" {
			continue
		}
		name, _, _ := strings.Cut(rest, "/")
		e, ok := seen[name]
		if !ok {
			e = &fsEntry{Name: name}
			seen[name] = e
		}
		if rest == name {
			// f.Path is exactly prefix+name: a leaf file of this directory.
			e.IsDir = false
			e.Path = f.Path
			e.Size = f.Size
		} else {
			// Deeper files share this first segment: it is a subdirectory.
			e.IsDir = true
		}
	}
	out := make([]fsEntry, 0, len(seen))
	for _, e := range seen {
		out = append(out, *e)
	}
	slices.SortFunc(out, func(a, b fsEntry) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// lookupChild returns the child named name of the torrent directory at
// relPrefix, reporting whether it exists.
func lookupChild(files []FileView, relPrefix, name string) (fsEntry, bool) {
	for _, e := range childrenOf(files, relPrefix) {
		if e.Name == name {
			return e, true
		}
	}
	return fsEntry{}, false
}

// rootEntryKind says what one immediate child of the mount root is: a torrent
// node (a regular file for a single-file torrent, a directory otherwise), the
// metadata control directory, or the stats control directory.
type rootEntryKind uint8

const (
	rootTorrentKind rootEntryKind = iota
	rootMetadataKind
	rootStatsKind
)

// rootEntry is one immediate child of the mount root.
type rootEntry struct {
	Name string
	Kind rootEntryKind
	View TorrentView
}

// isDir reports whether the entry is a directory entry. Only a single-file
// torrent root is a regular file; every control directory and every
// multi-file torrent root is a directory.
func (e rootEntry) isDir() bool {
	if e.Kind != rootTorrentKind {
		return true
	}
	_, single := mediaRoot(e.View)
	return !single
}

// mediaRoot returns the file a torrent view exposes directly at the mount
// root, when it has one. A single-file torrent (and only that) is exposed as
// one regular file; mislabelled views without exactly one file fall back to
// the directory layout.
func mediaRoot(view TorrentView) (FileView, bool) {
	if view.SingleFile && len(view.Files) == 1 {
		return view.Files[0], true
	}
	return FileView{}, false
}

// reservedRootNames are mount-root names the control directories occupy. A
// torrent whose display name collides with one is disambiguated by the same
// hash-suffix rule as any other duplicate.
func reservedRootNames() map[string]bool {
	return map[string]bool{metadataName: true, statsRootName: true}
}

// rootEntries returns the sorted top-level torrent entries, one per torrent.
// Torrents sharing a display name are disambiguated by appending a hash
// prefix to the later ones. Assignment order is deterministic (torrents are
// visited in hash order) so the same set always maps to the same names. The
// names are shared with the mirrored stats/ tree, which derives its layout
// from the same function.
func rootEntries(views []TorrentView) []rootEntry {
	ordered := slices.Clone(views)
	slices.SortFunc(ordered, func(a, b TorrentView) int {
		return strings.Compare(a.Hash.HexString(), b.Hash.HexString())
	})
	used := reservedRootNames()
	entries := make([]rootEntry, 0, len(ordered))
	for _, v := range ordered {
		name := uniqueTorrentName(v, used)
		entries = append(entries, rootEntry{Name: name, Kind: rootTorrentKind, View: v})
	}
	slices.SortFunc(entries, func(a, b rootEntry) int {
		return strings.Compare(a.Name, b.Name)
	})
	return entries
}

// uniqueTorrentName returns a mount root child name for v that is not in
// used, and records it. The torrent's own name is preferred; on collision a
// hash prefix is appended, extended until the name is free.
func uniqueTorrentName(v TorrentView, used map[string]bool) string {
	if !used[v.Name] {
		used[v.Name] = true
		return v.Name
	}
	hex := v.Hash.HexString()
	for i := 8; i <= len(hex); i += 4 {
		if cand := fmt.Sprintf("%s-%s", v.Name, hex[:i]); !used[cand] {
			used[cand] = true
			return cand
		}
	}
	for i := 0; ; i++ {
		if cand := fmt.Sprintf("%s-%s-%d", v.Name, hex, i); !used[cand] {
			used[cand] = true
			return cand
		}
	}
}

// metadataKey is the inode identity of the metadata control directory.
func metadataKey() string { return "metadata" }

// statsKey is the inode identity of the stats control directory.
func statsKey() string { return "stats" }

// torrentKey is the inode identity of a torrent's top directory.
func torrentKey(hash metainfo.Hash) string { return "t/" + hash.HexString() }

// fileKey is the inode identity of a leaf file inside a torrent.
func fileKey(hash metainfo.Hash, displayPath string) string {
	return "f/" + hash.HexString() + "/" + displayPath
}

// statsDirKey is the inode identity of a mirrored directory inside stats/.
func statsDirKey(hash metainfo.Hash, relPrefix string) string {
	return "sd/" + hash.HexString() + "/" + relPrefix
}

// statsFileKey is the inode identity of a mirrored status file inside stats/.
func statsFileKey(hash metainfo.Hash, displayPath string) string {
	return "sf/" + hash.HexString() + "/" + displayPath
}

// dirKey is the inode identity of a (virtual) subdirectory inside a torrent.
func dirKey(hash metainfo.Hash, relPrefix string) string {
	return "d/" + hash.HexString() + "/" + relPrefix
}

// joinRel joins a child name onto a torrent-relative directory prefix.
func joinRel(relPrefix, name string) string {
	if relPrefix == "" {
		return name
	}
	return relPrefix + "/" + name
}
