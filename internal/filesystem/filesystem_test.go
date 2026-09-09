package filesystem

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// fakeBackend is an in-memory Backend: no network, no anacrolix client, no
// real mount. Data is stored per (hash, display path).
type fakeBackend struct {
	views     []TorrentView
	data      map[string][]byte
	states    map[string][]PieceState
	statesErr error
}

func (b *fakeBackend) Torrents() []TorrentView { return b.views }

func (b *fakeBackend) OpenFile(hash metainfo.Hash, path string) (io.ReaderAt, error) {
	data, ok := b.data[hash.HexString()+"\x00"+path]
	if !ok {
		return nil, fmt.Errorf("fake backend: no file %q", path)
	}
	return bytes.NewReader(data), nil
}

func (b *fakeBackend) PieceStates(hash metainfo.Hash) ([]PieceState, error) {
	if b.statesErr != nil {
		return nil, b.statesErr
	}
	return append([]PieceState(nil), b.states[hash.HexString()]...), nil
}

func hashN(n byte) metainfo.Hash {
	var h metainfo.Hash
	h[0] = n
	return h
}

func (b *fakeBackend) addFile(t *testing.T, view *TorrentView, path string, data []byte) {
	t.Helper()
	view.Files = append(view.Files, FileView{Path: path, Size: int64(len(data))})
	if b.data == nil {
		b.data = make(map[string][]byte)
	}
	b.data[view.Hash.HexString()+"\x00"+path] = data
}

func namesOf(files []FileView) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

func entryNames(entries []fsEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

func TestChildrenOfSingleFile(t *testing.T) {
	files := []FileView{{Path: "blob.bin", Size: 11}}
	got := childrenOf(files, "")
	if want := []string{"blob.bin"}; !reflect.DeepEqual(entryNames(got), want) {
		t.Fatalf("children = %v, want %v", entryNames(got), want)
	}
	e := got[0]
	if e.IsDir || e.Path != "blob.bin" || e.Size != 11 {
		t.Fatalf("entry = %+v, want file blob.bin of size 11", e)
	}
}

func TestChildrenOfNested(t *testing.T) {
	files := []FileView{
		{Path: "a.txt", Size: 1},
		{Path: "sub/b.txt", Size: 2},
		{Path: "sub/deep/c.txt", Size: 3},
		{Path: "sub/deep/d.txt", Size: 4},
	}
	root := childrenOf(files, "")
	if want := []string{"a.txt", "sub"}; !reflect.DeepEqual(entryNames(root), want) {
		t.Fatalf("root children = %v, want %v", entryNames(root), want)
	}
	if !root[1].IsDir {
		t.Fatalf("sub should be a directory: %+v", root[1])
	}
	sub := childrenOf(files, "sub")
	if want := []string{"b.txt", "deep"}; !reflect.DeepEqual(entryNames(sub), want) {
		t.Fatalf("sub children = %v, want %v", entryNames(sub), want)
	}
	if sub[0].IsDir || sub[0].Path != "sub/b.txt" || sub[0].Size != 2 {
		t.Fatalf("b.txt entry wrong: %+v", sub[0])
	}
	deep := childrenOf(files, "sub/deep")
	if want := []string{"c.txt", "d.txt"}; !reflect.DeepEqual(entryNames(deep), want) {
		t.Fatalf("deep children = %v, want %v", entryNames(deep), want)
	}
}

func TestChildrenOfDoesNotLeakSiblingPrefixes(t *testing.T) {
	files := []FileView{
		{Path: "submarine/readme", Size: 1},
		{Path: "sub/b.txt", Size: 2},
	}
	got := childrenOf(files, "sub")
	if want := []string{"b.txt"}; !reflect.DeepEqual(entryNames(got), want) {
		t.Fatalf("children under sub = %v, want %v", entryNames(got), want)
	}
}

func TestChildrenOfEmpty(t *testing.T) {
	if got := childrenOf(nil, ""); len(got) != 0 {
		t.Fatalf("children = %v, want none", got)
	}
}

func TestLookupChild(t *testing.T) {
	files := []FileView{{Path: "a.txt", Size: 1}, {Path: "sub/b.txt", Size: 2}}
	if _, ok := lookupChild(files, "", "a.txt"); !ok {
		t.Fatal("a.txt should be found at root")
	}
	e, ok := lookupChild(files, "", "sub")
	if !ok || !e.IsDir {
		t.Fatalf("sub should be found as a dir: %+v ok=%v", e, ok)
	}
	if _, ok := lookupChild(files, "sub", "b.txt"); !ok {
		t.Fatal("b.txt should be found under sub")
	}
	if _, ok := lookupChild(files, "sub", "a.txt"); ok {
		t.Fatal("a.txt must not leak under sub")
	}
	if _, ok := lookupChild(files, "", "missing"); ok {
		t.Fatal("missing must not be found")
	}
}

func TestRootEntriesDeduplicatesNames(t *testing.T) {
	h1, h2 := hashN(0x01), hashN(0x02)
	views := []TorrentView{
		{Name: "dup", Hash: h1},
		{Name: "dup", Hash: h2},
	}
	entries := rootEntries(views)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	// Names are unique and sorted; exactly one keeps the plain name and the
	// other carries a hash-prefix suffix.
	names := []string{entries[0].Name, entries[1].Name}
	if names[0] == names[1] {
		t.Fatalf("entry names are not unique: %v", names)
	}
	if names[0] > names[1] {
		t.Fatalf("names not sorted: %v", names)
	}
	plain := 0
	for _, n := range names {
		if n == "dup" {
			plain++
		} else if !strings.HasPrefix(n, "dup-") {
			t.Fatalf("unexpected disambiguated name %q", n)
		}
	}
	if plain != 1 {
		t.Fatalf("plain name count = %d, want 1 in %v", plain, names)
	}
}

func TestRootEntriesSortsByName(t *testing.T) {
	views := []TorrentView{
		{Name: "zulu", Hash: hashN(0x01)},
		{Name: "alpha", Hash: hashN(0x02)},
	}
	entries := rootEntries(views)
	if want := []string{"alpha", "zulu"}; !reflect.DeepEqual([]string{entries[0].Name, entries[1].Name}, want) {
		t.Fatalf("names = %v, want %v", []string{entries[0].Name, entries[1].Name}, want)
	}
}

func TestNodeGetattr(t *testing.T) {
	ctx := context.Background()
	b := &fakeBackend{views: []TorrentView{{
		Name: "t", Hash: hashN(0x01), Files: []FileView{{Path: "f.bin", Size: 7}},
	}}}
	state := &fsState{backend: b}

	root := &rootNode{state: state}
	var dirAttr fuse.AttrOut
	if errno := root.Getattr(ctx, nil, &dirAttr); errno != 0 {
		t.Fatalf("root Getattr errno = %v", errno)
	}
	if dirAttr.Mode&0o7777 != 0o555 {
		t.Fatalf("root mode = %o, want 0555", dirAttr.Mode)
	}

	dir := &torrentDirNode{state: state, hash: hashN(0x01), files: b.views[0].Files}
	var dAttr fuse.AttrOut
	if errno := dir.Getattr(ctx, nil, &dAttr); errno != 0 {
		t.Fatalf("dir Getattr errno = %v", errno)
	}
	if dAttr.Mode&0o7777 != 0o555 {
		t.Fatalf("dir mode = %o, want 0555", dAttr.Mode)
	}

	file := &torrentFileNode{state: state, hash: hashN(0x01), path: "f.bin", size: 7}
	var fAttr fuse.AttrOut
	if errno := file.Getattr(ctx, nil, &fAttr); errno != 0 {
		t.Fatalf("file Getattr errno = %v", errno)
	}
	if fAttr.Mode&0o7777 != 0o444 {
		t.Fatalf("file mode = %o, want 0444", fAttr.Mode)
	}
	if fAttr.Size != 7 {
		t.Fatalf("file size = %d, want 7", fAttr.Size)
	}
}

func TestOpenRejectsWrite(t *testing.T) {
	ctx := context.Background()
	b := &fakeBackend{views: []TorrentView{{Name: "t", Hash: hashN(0x01)}}}
	b.addFile(t, &b.views[0], "f.bin", []byte("hello world"))
	state := &fsState{backend: b}
	file := &torrentFileNode{state: state, hash: b.views[0].Hash, path: "f.bin", size: 11}

	for _, flags := range []uint32{syscall.O_WRONLY, syscall.O_RDWR, syscall.O_WRONLY | syscall.O_TRUNC, syscall.O_RDONLY | syscall.O_TRUNC} {
		if fh, _, errno := file.Open(ctx, flags); errno != syscall.EROFS {
			t.Fatalf("Open(0x%x) = fh %v errno %v, want EROFS", flags, fh, errno)
		}
	}
	fh, fuseFlags, errno := file.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open(O_RDONLY) errno = %v, want 0", errno)
	}
	if fuseFlags != 0 {
		t.Fatalf("Open(O_RDONLY) fuseFlags = %v, want 0", fuseFlags)
	}
	rh, ok := fh.(*readHandle)
	if !ok {
		t.Fatalf("handle = %T, want *readHandle", fh)
	}
	res, errno := rh.Read(ctx, make([]byte, 32), 0)
	if errno != 0 {
		t.Fatalf("Read errno = %v", errno)
	}
	got, status := res.Bytes(nil)
	if status != fuse.OK {
		t.Fatalf("Read status = %v", status)
	}
	if string(got) != "hello world" {
		t.Fatalf("Read = %q, want %q", got, "hello world")
	}
}

func TestOpenMissingFile(t *testing.T) {
	ctx := context.Background()
	b := &fakeBackend{views: []TorrentView{{Name: "t", Hash: hashN(0x01)}}}
	state := &fsState{backend: b}
	file := &torrentFileNode{state: state, hash: b.views[0].Hash, path: "nope", size: 0}
	if _, _, errno := file.Open(ctx, syscall.O_RDONLY); errno != syscall.EIO {
		t.Fatalf("Open(missing) errno = %v, want EIO", errno)
	}
}

func TestReadHandleReadsAtOffsets(t *testing.T) {
	ctx := context.Background()
	data := []byte("hello brave new world")
	h := &readHandle{ra: bytes.NewReader(data), size: int64(len(data))}

	cases := []struct {
		off  int64
		n    int
		want string
	}{
		{0, len(data), string(data)},
		{0, 5, "hello"},
		{6, 5, "brave"},
		{int64(len(data) - 5), 32, "world"}, // crosses EOF: returns short
		{int64(len(data)), 32, ""},          // at EOF: empty result
		{int64(len(data) + 10), 32, ""},     // beyond EOF: empty result
	}
	for _, c := range cases {
		dest := make([]byte, c.n)
		res, errno := h.Read(ctx, dest, c.off)
		if errno != 0 {
			t.Fatalf("Read(off=%d) errno = %v", c.off, errno)
		}
		got, status := res.Bytes(nil)
		if status != fuse.OK {
			t.Fatalf("Read(off=%d) status = %v", c.off, status)
		}
		if string(got) != c.want {
			t.Fatalf("Read(off=%d) = %q, want %q", c.off, got, c.want)
		}
	}
}

func TestReadHandleNegativeOffset(t *testing.T) {
	h := &readHandle{ra: bytes.NewReader([]byte("x")), size: 1}
	if _, errno := h.Read(context.Background(), make([]byte, 4), -1); errno != syscall.EINVAL {
		t.Fatalf("Read(off=-1) errno = %v, want EINVAL", errno)
	}
}

func TestReaddirStream(t *testing.T) {
	ctx := context.Background()
	files := []FileView{
		{Path: "b.txt", Size: 2},
		{Path: "a.txt", Size: 1},
		{Path: "sub/c.txt", Size: 3},
	}
	dir := &torrentDirNode{files: files, prefix: ""}
	stream, errno := dir.Readdir(ctx)
	if errno != 0 {
		t.Fatalf("Readdir errno = %v", errno)
	}
	want := []string{".stats", "a.txt", "b.txt", "sub"}
	var got []fuse.DirEntry
	for stream.HasNext() {
		e, errno := stream.Next()
		if errno != 0 {
			t.Fatalf("Next errno = %v", errno)
		}
		got = append(got, e)
	}
	if len(got) != len(want) {
		t.Fatalf("Readdir names = %v, want %v", got, want)
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("Readdir[%d] = %q, want %q", i, got[i].Name, name)
		}
	}
	if got[0].Mode&syscall.S_IFMT != syscall.S_IFREG || got[3].Mode&syscall.S_IFMT != syscall.S_IFDIR {
		t.Fatalf("entry modes wrong: %v", got)
	}
}

func TestStatsFileRendersSnapshotAndReservesName(t *testing.T) {
	ctx := context.Background()
	hash := hashN(0x09)
	b := &fakeBackend{
		views: []TorrentView{{
			Name: "t",
			Hash: hash,
			Files: []FileView{
				{Path: ".stats", Size: 99},
				{Path: "sub/file", Size: 4},
			},
		}},
		states: map[string][]PieceState{
			hash.HexString(): {
				{Known: true, Complete: true},
				{Known: true, Partial: true, Bytes: 3},
				{Known: true},
				{Known: true, Wanted: true},
			},
		},
	}
	state := &fsState{backend: b, inoByKey: make(map[string]uint64), nextIno: 1}
	dir := &torrentDirNode{state: state, hash: hash, files: b.views[0].Files}
	entries := dir.entries()
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	if want := []string{".stats", "sub"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("top-level entries = %v, want %v", names, want)
	}
	if _, errno := (&torrentDirNode{state: state, hash: hash, files: b.views[0].Files, prefix: "sub"}).Lookup(ctx, ".stats", &fuse.EntryOut{}); errno != syscall.ENOENT {
		t.Fatalf("nested .stats lookup errno = %v, want ENOENT", errno)
	}

	stats := &statsFileNode{state: state, hash: hash}
	var attr fuse.AttrOut
	if errno := stats.Getattr(ctx, nil, &attr); errno != 0 {
		t.Fatalf("Getattr errno = %v", errno)
	}
	want := "[x] [X 3] [N] []\n"
	if attr.Size != uint64(len(want)) {
		t.Fatalf("attr size = %d, want %d", attr.Size, len(want))
	}
	fh, _, errno := stats.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open errno = %v", errno)
	}
	oldHandle := fh.(*readHandle)
	b.states[hash.HexString()] = []PieceState{{Known: true, Complete: true}}
	read := make([]byte, len(want))
	result, errno := oldHandle.Read(ctx, read, 0)
	if errno != 0 {
		t.Fatalf("snapshot Read errno = %v", errno)
	}
	got, status := result.Bytes(nil)
	if status != fuse.OK || string(got) != want {
		t.Fatalf("snapshot = %q, status %v; want %q", got, status, want)
	}

	newHandle, _, errno := stats.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("second Open errno = %v", errno)
	}
	newResult, errno := newHandle.(*readHandle).Read(ctx, make([]byte, 32), 0)
	if errno != 0 {
		t.Fatalf("second Read errno = %v", errno)
	}
	newGot, status := newResult.Bytes(nil)
	if status != fuse.OK || string(newGot) != "[x]\n" {
		t.Fatalf("new snapshot = %q, status %v; want [x]", newGot, status)
	}
}

func TestStatsFileRejectsWriteAndMapsBackendError(t *testing.T) {
	ctx := context.Background()
	hash := hashN(0x0a)
	b := &fakeBackend{statesErr: errors.New("backend failed")}
	stats := &statsFileNode{state: &fsState{backend: b}, hash: hash}
	var attr fuse.AttrOut
	if errno := stats.Getattr(ctx, nil, &attr); errno != syscall.EIO {
		t.Fatalf("Getattr errno = %v, want EIO", errno)
	}
	if _, _, errno := stats.Open(ctx, syscall.O_RDONLY); errno != syscall.EIO {
		t.Fatalf("Open backend error = %v, want EIO", errno)
	}
	for _, flags := range []uint32{syscall.O_WRONLY, syscall.O_RDWR, syscall.O_RDONLY | syscall.O_TRUNC, syscall.O_RDONLY | syscall.O_CREAT} {
		if _, _, errno := stats.Open(ctx, flags); errno != syscall.EROFS {
			t.Fatalf("Open(0x%x) = %v, want EROFS", flags, errno)
		}
	}
}

func TestHasMountOption(t *testing.T) {
	empty := &fs.Options{}
	if hasMountOption(empty, "ro") {
		t.Fatal("ro must not be reported present on empty options")
	}
	withRO := &fs.Options{MountOptions: fuse.MountOptions{Options: []string{"fsname=torrentfs", "ro"}}}
	if !hasMountOption(withRO, "ro") {
		t.Fatal("ro must be detected among other options")
	}
}

func TestMetadataRenameDropsOldInodeKey(t *testing.T) {
	state := &fsState{
		nextIno:       1,
		inoByKey:      make(map[string]uint64),
		metadataNodes: make(map[string]*metadataFileNode),
	}
	node := &metadataFileNode{name: "a.torrent"}
	state.rememberMetadataNode("a.torrent", node)
	oldIno := state.inoFor(metadataFileKey("a.torrent"))

	state.renameMetadataNode("a.torrent", "b.torrent")

	if got := state.inoFor(metadataFileKey("b.torrent")); got != oldIno {
		t.Fatalf("renamed inode = %d, want %d", got, oldIno)
	}
	newIno := state.inoFor(metadataFileKey("a.torrent"))
	if newIno == oldIno {
		t.Fatalf("recreated old name reused inode %d", newIno)
	}
	if newIno <= oldIno {
		t.Fatalf("recreated old name inode = %d, want monotonic value after %d", newIno, oldIno)
	}

	state.mu.Lock()
	if _, ok := state.metadataNodes["a.torrent"]; ok {
		t.Fatal("old metadata node name was retained after rename")
	}
	if got := state.metadataNodes["b.torrent"]; got != node {
		t.Fatalf("renamed metadata node = %p, want %p", got, node)
	}
	state.mu.Unlock()
	node.mu.Lock()
	name := node.name
	node.mu.Unlock()
	if name != "b.torrent" {
		t.Fatalf("renamed node name = %q, want b.torrent", name)
	}
}

// Interface assertions: the node types expose what go-fuse expects.
var (
	_ fs.NodeGetattrer = (*rootNode)(nil)
	_ fs.NodeLookuper  = (*rootNode)(nil)
	_ fs.NodeReaddirer = (*rootNode)(nil)
	_ fs.NodeGetattrer = (*torrentDirNode)(nil)
	_ fs.NodeLookuper  = (*torrentDirNode)(nil)
	_ fs.NodeReaddirer = (*torrentDirNode)(nil)
	_ fs.NodeGetattrer = (*torrentFileNode)(nil)
	_ fs.NodeOpener    = (*torrentFileNode)(nil)
)
