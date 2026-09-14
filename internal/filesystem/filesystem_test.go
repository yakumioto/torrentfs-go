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
	views      []TorrentView
	data       map[string][]byte
	states     map[string][]PieceState
	fileStates map[string][]PieceState
	statesErr  error
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

// FilePieceStates returns the per-file projection when one was registered and
// otherwise falls back to the whole-torrent snapshot, so a test that only
// cares about node wiring can leave the projection unset.
func (b *fakeBackend) FilePieceStates(hash metainfo.Hash, path string) ([]PieceState, error) {
	if b.statesErr != nil {
		return nil, b.statesErr
	}
	if states, ok := b.fileStates[hash.HexString()+"\x00"+path]; ok {
		return append([]PieceState(nil), states...), nil
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
	want := []string{"a.txt", "b.txt", "sub"}
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
	if got[0].Mode&syscall.S_IFMT != syscall.S_IFREG || got[2].Mode&syscall.S_IFMT != syscall.S_IFDIR {
		t.Fatalf("entry modes wrong: %v", got)
	}
}

func TestTorrentDirHasNoStatsEntry(t *testing.T) {
	ctx := context.Background()
	hash := hashN(0x09)
	files := []FileView{
		{Path: ".stats", Size: 99}, // an ordinary data file, not a reserved name
		{Path: "sub/file", Size: 4},
	}
	dir := &torrentDirNode{hash: hash, files: files}
	var names []string
	for _, entry := range dir.entries() {
		names = append(names, entry.Name)
	}
	if want := []string{".stats", "sub"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("top-level entries = %v, want %v", names, want)
	}
	if _, errno := (&torrentDirNode{hash: hash, files: files, prefix: "sub"}).Lookup(ctx, ".stats", &fuse.EntryOut{}); errno != syscall.ENOENT {
		t.Fatalf("nested .stats lookup errno = %v, want ENOENT", errno)
	}
}

func TestMediaRootClassification(t *testing.T) {
	one := FileView{Path: "payload.bin", Size: 11}
	single := TorrentView{Name: "p", Hash: hashN(0x01), Files: []FileView{one}, SingleFile: true}
	if f, ok := mediaRoot(single); !ok || f.Path != "payload.bin" {
		t.Fatalf("mediaRoot(single) = %+v, %v; want payload.bin", f, ok)
	}
	// A one-file multi-file torrent (SingleFile false) is still a directory.
	oneFileMulti := TorrentView{Name: "p", Hash: hashN(0x01), Files: []FileView{one}}
	if _, ok := mediaRoot(oneFileMulti); ok {
		t.Fatal("one-file multi-file torrent must not be exposed as a regular file")
	}
	// A view flagged single-file without exactly one file falls back to a
	// directory rather than fabricating a media path.
	if _, ok := mediaRoot(TorrentView{Name: "p", Hash: hashN(0x01), SingleFile: true}); ok {
		t.Fatal("single-file view without one file must fall back")
	}
	two := TorrentView{Name: "p", Hash: hashN(0x01), Files: []FileView{one, {Path: "b", Size: 1}}, SingleFile: true}
	if _, ok := mediaRoot(two); ok {
		t.Fatal("view with two files must fall back to a directory")
	}
}

func TestRootEntryIsDir(t *testing.T) {
	single := rootEntry{Name: "a", Kind: rootTorrentKind, View: TorrentView{SingleFile: true, Files: []FileView{{Path: "a", Size: 1}}}}
	if single.isDir() {
		t.Fatal("single-file torrent root must be a regular file")
	}
	multi := rootEntry{Name: "a", Kind: rootTorrentKind, View: TorrentView{Files: []FileView{{Path: "a/b", Size: 1}}}}
	if !multi.isDir() {
		t.Fatal("multi-file torrent root must be a directory")
	}
	for _, kind := range []rootEntryKind{rootMetadataKind, rootStatsKind} {
		if !(&rootEntry{Kind: kind}).isDir() {
			t.Fatalf("control entry kind %d must be a directory", kind)
		}
	}
}

func TestRootEntriesReserveControlNames(t *testing.T) {
	views := []TorrentView{
		{Name: "stats", Hash: hashN(0x01)},
		{Name: "metadata", Hash: hashN(0x02)},
	}
	entries := rootEntries(views)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	names := []string{entries[0].Name, entries[1].Name}
	if names[0] > names[1] {
		t.Fatalf("entries not sorted: %v", names)
	}
	for _, name := range names {
		if name == statsRootName || name == metadataName {
			t.Fatalf("torrent name collided with a control name: %v", names)
		}
	}
}

func TestStatsFileRendersFileSnapshot(t *testing.T) {
	ctx := context.Background()
	hash := hashN(0x0b)
	states := []PieceState{
		{Known: true, Complete: true},
		{Known: true, Partial: true, Bytes: 3},
		{Known: true},
		{Known: true, Wanted: true},
	}
	b := &fakeBackend{
		states:     map[string][]PieceState{hash.HexString(): {{Known: true, Complete: true}}},
		fileStates: map[string][]PieceState{hash.HexString() + "\x00sub/file": states},
	}
	state := newFSState(b)
	stats := &statsFileNode{state: state, hash: hash, path: "sub/file"}

	want := "[x] [X 3] [N] []\n"
	var attr fuse.AttrOut
	if errno := stats.Getattr(ctx, nil, &attr); errno != 0 {
		t.Fatalf("Getattr errno = %v", errno)
	}
	if attr.Mode&0o7777 != 0o444 {
		t.Fatalf("stats file mode = %o, want 0444", attr.Mode)
	}
	if attr.Size != uint64(len(want)) {
		t.Fatalf("attr size = %d, want %d", attr.Size, len(want))
	}

	fh, _, errno := stats.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open errno = %v", errno)
	}
	oldHandle := fh.(*readHandle)
	// The open snapshot is captured at open time; later state changes are not
	// visible through the existing handle.
	b.fileStates[hash.HexString()+"\x00sub/file"] = []PieceState{{Known: true, Complete: true}}
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
	stats := &statsFileNode{state: newFSState(b), hash: hash, path: "f"}
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

// bridgedRoot attaches root to a go-fuse bridge so node constructors such as
// NewInode work in unit tests without a kernel mount.
func bridgedRoot(t *testing.T, root fs.InodeEmbedder) {
	t.Helper()
	if fs.NewNodeFS(root, nil) == nil {
		t.Fatal("NewNodeFS returned nil")
	}
}

func namedEntry(t *testing.T, stream fs.DirStream) []fuse.DirEntry {
	t.Helper()
	var got []fuse.DirEntry
	for stream.HasNext() {
		e, errno := stream.Next()
		if errno != 0 {
			t.Fatalf("Next errno = %v", errno)
		}
		got = append(got, e)
	}
	return got
}

func TestRootLookupSingleFileIsRegularFile(t *testing.T) {
	ctx := context.Background()
	hash := hashN(0x21)
	content := []byte("hello world")
	b := &fakeBackend{
		views: []TorrentView{{
			Name:       "payload.bin",
			Hash:       hash,
			SingleFile: true,
			Files:      []FileView{{Path: "payload.bin", Size: int64(len(content))}},
		}},
		data: map[string][]byte{hash.HexString() + "\x00payload.bin": content},
	}
	root := &rootNode{state: newFSState(b)}
	bridgedRoot(t, root)

	// Readdir marks the single-file torrent root as a regular file and still
	// lists the stats control directory.
	stream, errno := root.Readdir(ctx)
	if errno != 0 {
		t.Fatalf("Readdir errno = %v", errno)
	}
	entries := namedEntry(t, stream)
	if len(entries) != 2 || entries[0].Name != "payload.bin" || entries[1].Name != statsRootName {
		t.Fatalf("root entries = %v, want payload.bin and stats", entries)
	}
	if entries[0].Mode&syscall.S_IFMT != syscall.S_IFREG {
		t.Fatalf("payload.bin mode = %o, want regular file", entries[0].Mode)
	}
	if entries[1].Mode&syscall.S_IFMT != syscall.S_IFDIR {
		t.Fatalf("stats mode = %o, want directory", entries[1].Mode)
	}

	var entry fuse.EntryOut
	inode, errno := root.Lookup(ctx, "payload.bin", &entry)
	if errno != 0 {
		t.Fatalf("Lookup(payload.bin) errno = %v", errno)
	}
	// Lookup's EntryOut carries permission bits; the file type lives in the
	// StableAttr the bridge hands the kernel.
	if inode.StableAttr().Mode&syscall.S_IFMT != syscall.S_IFREG {
		t.Fatalf("payload.bin inode mode = %o, want regular file", inode.StableAttr().Mode)
	}
	if entry.Size != uint64(len(content)) {
		t.Fatalf("payload.bin entry size = %d, want %d", entry.Size, len(content))
	}
	file, ok := inode.Operations().(*torrentFileNode)
	if !ok {
		t.Fatalf("payload.bin node = %T, want *torrentFileNode", inode.Operations())
	}
	if file.path != "payload.bin" {
		t.Fatalf("media node path = %q, want the original display path", file.path)
	}
	handle, _, errno := file.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open media file errno = %v", errno)
	}
	got, status := mustRead(t, ctx, handle.(*readHandle), len(content))
	if status != fuse.OK || string(got) != string(content) {
		t.Fatalf("media read = %q, status %v; want %q", got, status, content)
	}
}

// mustRead reads exactly n bytes from a read handle starting at offset 0.
func mustRead(t *testing.T, ctx context.Context, h *readHandle, n int) ([]byte, fuse.Status) {
	t.Helper()
	res, errno := h.Read(ctx, make([]byte, n), 0)
	if errno != 0 {
		t.Fatalf("Read errno = %v", errno)
	}
	return res.Bytes(nil)
}

func TestStatsRootMirrorsDataTree(t *testing.T) {
	ctx := context.Background()
	singleHash := hashN(0x31)
	multiHash := hashN(0x32)
	b := &fakeBackend{
		views: []TorrentView{
			{
				Name:       "payload.bin",
				Hash:       singleHash,
				SingleFile: true,
				Files:      []FileView{{Path: "payload.bin", Size: 4}},
			},
			{
				Name: "multi",
				Hash: multiHash,
				Files: []FileView{
					{Path: "a.txt", Size: 1},
					{Path: "sub/b.txt", Size: 2},
				},
			},
		},
		states: map[string][]PieceState{
			singleHash.HexString(): {{Known: true, Complete: true}},
			multiHash.HexString():  {{Known: true, Complete: true}},
		},
	}
	root := &rootNode{state: newFSState(b)}
	bridgedRoot(t, root)

	statsInode, errno := root.Lookup(ctx, statsRootName, &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("Lookup(stats) errno = %v", errno)
	}
	statsRoot, ok := statsInode.Operations().(*statsRootNode)
	if !ok {
		t.Fatalf("stats node = %T, want *statsRootNode", statsInode.Operations())
	}

	// The stats root mirrors the data tree's names and node types.
	stream, errno := statsRoot.Readdir(ctx)
	if errno != 0 {
		t.Fatalf("stats Readdir errno = %v", errno)
	}
	entries := namedEntry(t, stream)
	if len(entries) != 2 || entries[0].Name != "multi" || entries[1].Name != "payload.bin" {
		t.Fatalf("stats entries = %v, want multi and payload.bin", entries)
	}
	if entries[0].Mode&syscall.S_IFMT != syscall.S_IFDIR {
		t.Fatalf("stats multi mode = %o, want directory", entries[0].Mode)
	}
	if entries[1].Mode&syscall.S_IFMT != syscall.S_IFREG {
		t.Fatalf("stats payload.bin mode = %o, want regular file", entries[1].Mode)
	}

	// The single-file torrent yields one status leaf named for the media root.
	leafInode, errno := statsRoot.Lookup(ctx, "payload.bin", &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("stats Lookup(payload.bin) errno = %v", errno)
	}
	leaf, ok := leafInode.Operations().(*statsFileNode)
	if !ok || leaf.path != "payload.bin" {
		t.Fatalf("stats payload.bin node = %#v, want leaf for payload.bin", leafInode.Operations())
	}

	// The multi-file torrent mirrors its directory tree.
	multiInode, errno := statsRoot.Lookup(ctx, "multi", &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("stats Lookup(multi) errno = %v", errno)
	}
	multiDir, ok := multiInode.Operations().(*statsDirNode)
	if !ok {
		t.Fatalf("stats multi node = %T, want *statsDirNode", multiInode.Operations())
	}
	stream, errno = multiDir.Readdir(ctx)
	if errno != 0 {
		t.Fatalf("stats multi Readdir errno = %v", errno)
	}
	names := make([]string, 0, 2)
	for _, e := range namedEntry(t, stream) {
		names = append(names, e.Name)
	}
	if want := []string{"a.txt", "sub"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("stats multi entries = %v, want %v", names, want)
	}
	subInode, errno := multiDir.Lookup(ctx, "sub", &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("stats Lookup(sub) errno = %v", errno)
	}
	subDir, ok := subInode.Operations().(*statsDirNode)
	if !ok {
		t.Fatalf("stats sub node = %T, want *statsDirNode", subInode.Operations())
	}
	subLeaf, errno := subDir.Lookup(ctx, "b.txt", &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("stats Lookup(sub/b.txt) errno = %v", errno)
	}
	if leaf, ok := subLeaf.Operations().(*statsFileNode); !ok || leaf.path != "sub/b.txt" {
		t.Fatalf("stats sub/b.txt node = %#v, want leaf for sub/b.txt", subLeaf.Operations())
	}
}

func TestStatsTreeReadOnly(t *testing.T) {
	ctx := context.Background()
	hash := hashN(0x41)
	files := []FileView{{Path: "sub/b.txt", Size: 2}}
	state := newFSState(&fakeBackend{})
	root := &statsRootNode{state: state}
	dir := &statsDirNode{state: state, hash: hash, files: files}

	type writeOps struct {
		name string
		node interface {
			Mkdir(context.Context, string, uint32, *fuse.EntryOut) (*fs.Inode, syscall.Errno)
			Create(context.Context, string, uint32, uint32, *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno)
			Unlink(context.Context, string) syscall.Errno
			Rmdir(context.Context, string) syscall.Errno
		}
	}
	for _, tc := range []writeOps{{"stats root", root}, {"stats dir", dir}} {
		if _, errno := tc.node.Mkdir(ctx, "x", 0o755, &fuse.EntryOut{}); errno != syscall.EROFS {
			t.Errorf("%s mkdir = %v, want EROFS", tc.name, errno)
		}
		if _, _, _, errno := tc.node.Create(ctx, "x", 0, 0o644, &fuse.EntryOut{}); errno != syscall.EROFS {
			t.Errorf("%s create = %v, want EROFS", tc.name, errno)
		}
		if errno := tc.node.Unlink(ctx, "x"); errno != syscall.EROFS {
			t.Errorf("%s unlink = %v, want EROFS", tc.name, errno)
		}
		if errno := tc.node.Rmdir(ctx, "x"); errno != syscall.EROFS {
			t.Errorf("%s rmdir = %v, want EROFS", tc.name, errno)
		}
	}
	if errno := root.Rename(ctx, "x", root, "y", 0); errno != syscall.EROFS {
		t.Errorf("stats root rename = %v, want EROFS", errno)
	}
	if errno := dir.Rename(ctx, "x", dir, "y", 0); errno != syscall.EROFS {
		t.Errorf("stats dir rename = %v, want EROFS", errno)
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
