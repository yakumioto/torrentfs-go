package filesystem

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type fakeBackend struct {
	views []TorrentView
	data  map[string][]byte
}

func (b *fakeBackend) Torrents() []TorrentView { return b.views }

func (b *fakeBackend) OpenFile(hash metainfo.Hash, path string) (io.ReaderAt, error) {
	data, ok := b.data[hash.HexString()+"\x00"+path]
	if !ok {
		return nil, fmt.Errorf("fake backend: no file %q", path)
	}
	return bytes.NewReader(data), nil
}

func hashN(n byte) metainfo.Hash {
	var hash metainfo.Hash
	hash[0] = n
	return hash
}

func (b *fakeBackend) addFile(view *TorrentView, path string, data []byte) {
	view.Files = append(view.Files, FileView{Path: path, Size: int64(len(data))})
	if b.data == nil {
		b.data = make(map[string][]byte)
	}
	b.data[view.Hash.HexString()+"\x00"+path] = data
}

func entryNames(entries []fsEntry) []string {
	out := make([]string, len(entries))
	for i, entry := range entries {
		out[i] = entry.Name
	}
	return out
}

func TestChildrenOfNestedFiles(t *testing.T) {
	files := []FileView{
		{Path: "a.txt", Size: 1},
		{Path: "sub/b.txt", Size: 2},
		{Path: "sub/deep/c.txt", Size: 3},
	}
	root := childrenOf(files, "")
	if want := []string{"a.txt", "sub"}; !reflect.DeepEqual(entryNames(root), want) {
		t.Fatalf("root children = %v, want %v", entryNames(root), want)
	}
	if !root[1].IsDir {
		t.Fatalf("sub entry = %+v, want directory", root[1])
	}
	if want := []string{"b.txt", "deep"}; !reflect.DeepEqual(entryNames(childrenOf(files, "sub")), want) {
		t.Fatalf("sub children = %v, want %v", entryNames(childrenOf(files, "sub")), want)
	}
}

func TestRootEntriesExposeFormerControlNamesAsData(t *testing.T) {
	entries := rootEntries([]TorrentView{
		{Name: "metadata", Hash: hashN(1)},
		{Name: "stats", Hash: hashN(2)},
	})
	if want := []string{"metadata", "stats"}; !reflect.DeepEqual([]string{entries[0].Name, entries[1].Name}, want) {
		t.Fatalf("root entries = %+v, want %v", entries, want)
	}
}

func TestRootEntriesDisambiguateDuplicateNames(t *testing.T) {
	entries := rootEntries([]TorrentView{{Name: "dup", Hash: hashN(1)}, {Name: "dup", Hash: hashN(2)}})
	if len(entries) != 2 || entries[0].Name == entries[1].Name {
		t.Fatalf("entries = %+v, want two distinct names", entries)
	}
	plain := 0
	for _, entry := range entries {
		if entry.Name == "dup" {
			plain++
		} else if !strings.HasPrefix(entry.Name, "dup-") {
			t.Fatalf("entry name %q lacks duplicate suffix", entry.Name)
		}
	}
	if plain != 1 {
		t.Fatalf("plain duplicate count = %d, want 1", plain)
	}
}

func TestRootContainsOnlyTorrentDataAndIsReadOnly(t *testing.T) {
	backend := &fakeBackend{views: []TorrentView{
		{Name: "data", Hash: hashN(1), Files: []FileView{{Path: "payload", Size: 7}}, SingleFile: true},
	}}
	root := &rootNode{state: newFSState(backend)}
	entries := root.children()
	if len(entries) != 1 || entries[0].Name != "data" {
		t.Fatalf("root entries = %+v, want only data", entries)
	}
	var attr fuse.AttrOut
	if errno := root.Getattr(context.Background(), nil, &attr); errno != 0 {
		t.Fatalf("root Getattr errno = %v", errno)
	}
	if attr.Mode&0o7777 != 0o555 {
		t.Fatalf("root mode = %o, want 0555", attr.Mode)
	}
	if _, _, _, errno := root.Create(context.Background(), "new", 0, 0, &fuse.EntryOut{}); errno != syscall.EROFS {
		t.Fatalf("root Create errno = %v, want EROFS", errno)
	}
	if _, errno := root.Mkdir(context.Background(), "new", 0, &fuse.EntryOut{}); errno != syscall.EROFS {
		t.Fatalf("root Mkdir errno = %v, want EROFS", errno)
	}
}

func TestTorrentNodesAreReadOnlyAndOpenData(t *testing.T) {
	backend := &fakeBackend{views: []TorrentView{{Name: "data", Hash: hashN(1)}}}
	backend.addFile(&backend.views[0], "nested/payload", []byte("content"))
	state := newFSState(backend)
	dir := &torrentDirNode{state: state, hash: backend.views[0].Hash, files: backend.views[0].Files}
	if _, errno := dir.Mkdir(context.Background(), "new", 0, &fuse.EntryOut{}); errno != syscall.EROFS {
		t.Fatalf("Mkdir errno = %v, want EROFS", errno)
	}
	if errno := dir.Unlink(context.Background(), "nested"); errno != syscall.EROFS {
		t.Fatalf("Unlink errno = %v, want EROFS", errno)
	}

	node := &torrentFileNode{state: state, hash: backend.views[0].Hash, path: "nested/payload", size: 7}
	if _, _, errno := node.Open(context.Background(), syscall.O_WRONLY); errno != syscall.EROFS {
		t.Fatalf("write Open errno = %v, want EROFS", errno)
	}
	handle, _, errno := node.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("read Open errno = %v", errno)
	}
	read, ok := handle.(*readHandle)
	if !ok {
		t.Fatalf("handle = %T, want *readHandle", handle)
	}
	buf := make([]byte, 7)
	result, errno := read.Read(context.Background(), buf, 0)
	if errno != 0 {
		t.Fatalf("Read errno = %v", errno)
	}
	out, status := result.Bytes(buf)
	if status != fuse.OK || string(out) != "content" {
		t.Fatalf("read result = %q status=%v, want content/OK", out, status)
	}
}
