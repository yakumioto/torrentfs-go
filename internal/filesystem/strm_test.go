package filesystem

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// strmTestBackend reuses the payload and subtitle fixtures while recording the
// distinction between URL metadata and opening real payload data. Published
// view slices are immutable; concurrent changes replace the whole slice.
type strmTestBackend struct {
	*fakeBackend
	mu            sync.Mutex
	streamCalls   int
	payloadOpens  int
	torrentsCalls int
	streamErrors  map[string]error
	urlHook       func(metainfo.Hash, string) error
}

func (b *strmTestBackend) Torrents() []TorrentView {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.torrentsCalls++
	return b.views
}

// snapshots reports how often the whole-library snapshot was taken.
func (b *strmTestBackend) snapshots() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.torrentsCalls
}

func (b *strmTestBackend) Categories() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.categories)
}

func (b *strmTestBackend) OpenFile(hash metainfo.Hash, path string) (io.ReaderAt, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.payloadOpens++
	return b.fakeBackend.OpenFile(hash, path)
}

func (b *strmTestBackend) OpenSubtitle(hash metainfo.Hash, path string) (SubtitleSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fakeBackend.OpenSubtitle(hash, path)
}

func (b *strmTestBackend) SubtitleStat(hash metainfo.Hash, path string) (SubtitleStat, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fakeBackend.SubtitleStat(hash, path)
}

func (b *strmTestBackend) StreamURL(hash metainfo.Hash, path string) (string, error) {
	b.mu.Lock()
	b.streamCalls++
	hook := b.urlHook
	b.urlHook = nil
	b.mu.Unlock()
	if hook != nil {
		if err := hook(hash, path); err != nil {
			return "", err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.streamErrors[hash.HexString()+"\x00"+path]; err != nil {
		return "", err
	}
	for _, view := range b.views {
		if view.Hash != hash {
			continue
		}
		for _, file := range view.Files {
			if file.Path == path {
				return strmTestURL(hash, path), nil
			}
		}
	}
	return "", ErrNotFound
}

func (b *strmTestBackend) counts() (stream, payload int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.streamCalls, b.payloadOpens
}

func (b *strmTestBackend) setViews(views []TorrentView) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.views = views
}

func strmTestURL(hash metainfo.Hash, path string) string {
	parts := strings.Split(path, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return "http://media.test/stream/" + hash.HexString() + "/signature/" + strings.Join(parts, "/")
}

func strmTestRead(t *testing.T, reader io.ReaderAt) string {
	t.Helper()
	var out strings.Builder
	buf := make([]byte, 17)
	for offset := int64(0); ; {
		n, err := reader.ReadAt(buf, offset)
		out.Write(buf[:n])
		offset += int64(n)
		if err == io.EOF {
			return out.String()
		}
		if err != nil {
			t.Fatalf("ReadAt: %v", err)
		}
		if n == 0 {
			t.Fatal("ReadAt returned no data and no error")
		}
	}
}

func strmTestList(t *testing.T, backend Backend) []TorrentView {
	t.Helper()
	listed := make(chan []TorrentView, 1)
	go func() { listed <- backend.Torrents() }()
	select {
	case views := <-listed:
		return views
	case <-time.After(5 * time.Second):
		t.Fatal("STRM listing did not return")
		return nil
	}
}

func strmTestClone(views []TorrentView) []TorrentView {
	out := slices.Clone(views)
	for i := range out {
		out[i].Files = slices.Clone(out[i].Files)
		out[i].Subtitles = slices.Clone(out[i].Subtitles)
	}
	return out
}

func strmTestPaths(original, projected TorrentView) map[string]string {
	out := make(map[string]string, len(original.Files))
	for i := range original.Files {
		out[original.Files[i].Path] = projected.Files[i].Path
	}
	return out
}

func TestSTRMProjectionCopiesSnapshotsAndPreservesPayload(t *testing.T) {
	created := time.Date(2026, 10, 1, 2, 3, 4, 0, time.UTC)
	backend := &strmTestBackend{fakeBackend: &fakeBackend{views: []TorrentView{
		{Name: "Collection", Hash: hashN(1), Category: "movies", CreatedAt: created},
	}}}
	for _, path := range []string{
		"Movie.mkv", "Season 1/E 01.mp4", "BDMV/STREAM/00001.M2TS", "Disc.ISO",
		"Movie.srt", "existing.strm", "info/readme.txt",
	} {
		backend.addFile(&backend.views[0], path, []byte("payload:"+path))
	}
	backend.addSubtitle(&backend.views[0], "Season 1/E 01.zh.srt", "Season 1/E 01.mp4", []byte("subtitle"))
	wantOriginal := strmTestClone(backend.views)
	projection := NewSTRMBackend(backend)
	views := projection.Torrents()
	if len(views) != 1 || len(views[0].Files) != len(wantOriginal[0].Files) {
		t.Fatalf("projected views = %+v, want every payload file", views)
	}
	if !reflect.DeepEqual(backend.views, wantOriginal) {
		t.Fatal("projection mutated the original snapshot")
	}
	if views[0].Name != "Collection" || views[0].Category != "movies" || !views[0].CreatedAt.Equal(created) {
		t.Fatalf("torrent metadata changed: %+v", views[0])
	}
	if !reflect.DeepEqual(views[0].Subtitles, wantOriginal[0].Subtitles) {
		t.Fatalf("subtitles = %+v, want unchanged metadata", views[0].Subtitles)
	}
	wantPaths := []string{
		"Movie.strm", "Season 1/E 01.strm", "BDMV/STREAM/00001.strm", "Disc.iso.strm",
		"Movie.srt", "existing.strm", "info/readme.txt",
	}
	for i, file := range views[0].Files {
		if file.Path != wantPaths[i] {
			t.Fatalf("file %d path = %q, want %q", i, file.Path, wantPaths[i])
		}
		if i < 4 {
			want := strmTestURL(hashN(1), wantOriginal[0].Files[i].Path) + "\n"
			if file.Size != int64(len(want)) {
				t.Fatalf("%q size = %d, want %d bytes", file.Path, file.Size, len(want))
			}
			reader, err := projection.OpenFile(hashN(1), file.Path)
			if err != nil {
				t.Fatalf("OpenFile %q: %v", file.Path, err)
			}
			if got := strmTestRead(t, reader); got != want {
				t.Fatalf("%q content = %q, want %q", file.Path, got, want)
			}
		} else if file != wantOriginal[0].Files[i] {
			t.Fatalf("passthrough file = %+v, want %+v", file, wantOriginal[0].Files[i])
		}
	}
	if stream, payload := backend.counts(); stream != 8 || payload != 0 {
		t.Fatalf("calls after listing and generated opens = %d URL/%d payload, want 8/0", stream, payload)
	}
	for _, path := range wantPaths[4:] {
		reader, err := projection.OpenFile(hashN(1), path)
		if err != nil {
			t.Fatalf("OpenFile passthrough %q: %v", path, err)
		}
		if got := strmTestRead(t, reader); got != "payload:"+path {
			t.Fatalf("passthrough %q = %q", path, got)
		}
	}
	views[0].Files[0].Path = "changed"
	views[0].Subtitles[0].Path = "changed.srt"
	views[0].Subtitles[0].Size = 999
	if !reflect.DeepEqual(backend.views, wantOriginal) {
		t.Fatal("mutating the projected slices changed the original snapshot")
	}
}

func TestSTRMProjectionConvertsEveryVideoExtension(t *testing.T) {
	backend := &strmTestBackend{fakeBackend: &fakeBackend{views: []TorrentView{{Name: "videos", Hash: hashN(1)}}}}
	for i, ext := range []string{
		".3gp", ".avi", ".flv", ".m2ts", ".m4v", ".mkv", ".mov", ".mp4",
		".mpeg", ".mpg", ".mts", ".ts", ".webm", ".wmv", ".iso",
	} {
		backend.addFile(&backend.views[0], fmt.Sprintf("video-%d%s", i, strings.ToUpper(ext)), []byte("video"))
	}
	projection := NewSTRMBackend(backend)
	view := projection.Torrents()[0]
	for i, file := range view.Files {
		ending := ".strm"
		if i == len(view.Files)-1 {
			ending = ".iso.strm"
		}
		if want := fmt.Sprintf("video-%d%s", i, ending); file.Path != want {
			t.Errorf("%q projected as %q, want %q", backend.views[0].Files[i].Path, file.Path, want)
		}
	}
}

func TestSTRMNamesReservePayloadAndDirectories(t *testing.T) {
	backend := &strmTestBackend{fakeBackend: &fakeBackend{views: []TorrentView{{Name: "collection", Hash: hashN(1)}}}}
	paths := []string{
		"Movie.mkv", "Movie.mp4", "Movie.strm", "Movie.mkv.strm", "Movie.mp4.strm",
		"Dir.mkv", "Dir.strm/readme.txt", "Disc.iso", "Disc.iso.strm", "Other.ISO", ".mkv",
		"folder/Show.mkv", "folder/Show.mp4", "folder/Show.strm/readme.txt",
		"BDMV/STREAM/00001.m2ts", "中文/电影.mkv", "中文/电影.mp4",
	}
	for _, path := range paths {
		backend.addFile(&backend.views[0], path, []byte(path))
	}
	projection := NewSTRMBackend(backend)
	view := projection.Torrents()[0]
	if len(view.Files) != len(paths) {
		t.Fatalf("file count = %d, want %d", len(view.Files), len(paths))
	}
	gotPaths := strmTestPaths(backend.views[0], view)
	for original, want := range map[string]string{
		"Dir.mkv": "Dir.mkv.strm", "Other.ISO": "Other.iso.strm", ".mkv": ".strm",
		"folder/Show.mkv": "folder/Show.mkv.strm", "folder/Show.mp4": "folder/Show.mp4.strm",
		"BDMV/STREAM/00001.m2ts": "BDMV/STREAM/00001.strm", "中文/电影.mkv": "中文/电影.mkv.strm",
		"中文/电影.mp4": "中文/电影.mp4.strm",
	} {
		if gotPaths[original] != want {
			t.Errorf("%q -> %q, want %q", original, gotPaths[original], want)
		}
	}
	for _, original := range []string{"Movie.mkv", "Movie.mp4", "Disc.iso"} {
		path := gotPaths[original]
		if !strings.Contains(path, "-") || !strings.HasSuffix(path, ".strm") || path == original+".strm" {
			t.Errorf("%q -> %q, want hash disambiguation before .strm", original, path)
		}
		if original == "Disc.iso" && !strings.HasSuffix(path, ".iso.strm") {
			t.Errorf("ISO name %q lost .iso.strm ending", path)
		}
	}
	seen := make(map[string]bool)
	for i, file := range view.Files {
		if seen[file.Path] {
			t.Fatalf("duplicate projected path %q", file.Path)
		}
		seen[file.Path] = true
		original := paths[i]
		reader, err := projection.OpenFile(view.Hash, file.Path)
		if err != nil {
			t.Fatalf("OpenFile %q: %v", file.Path, err)
		}
		want := original
		if ext, _ := strmExtension(original); ext != "" {
			want = strmTestURL(view.Hash, original) + "\n"
		}
		if got := strmTestRead(t, reader); got != want {
			t.Fatalf("%q content = %q, want original identity %q", file.Path, got, want)
		}
	}
	if entry, ok := lookupChild(view.Files, "", "Dir.strm"); !ok || !entry.IsDir {
		t.Fatalf("reserved directory = %+v/%v, want directory", entry, ok)
	}
	if gotPaths["Movie.strm"] != "Movie.strm" || gotPaths["Disc.iso.strm"] != "Disc.iso.strm" {
		t.Fatal("preexisting STRM files were renamed")
	}
	reversed := strmTestClone(backend.views)
	slices.Reverse(reversed[0].Files)
	backend.setViews(reversed)
	if got := strmTestPaths(reversed[0], projection.Torrents()[0]); !reflect.DeepEqual(got, gotPaths) {
		t.Fatalf("input ordering changed names:\n%v\nwant %v", got, gotPaths)
	}
}

func TestSTRMNamesHandleFullHashAndSequenceCollisions(t *testing.T) {
	backend := &strmTestBackend{fakeBackend: &fakeBackend{views: []TorrentView{{Name: "collision", Hash: hashN(1)}}}}
	backend.addFile(&backend.views[0], "Movie.mkv", []byte("video"))
	backend.addFile(&backend.views[0], "Movie.strm", []byte("strm"))
	backend.addFile(&backend.views[0], "Movie.mkv.strm", []byte("strm"))
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte("Movie.mkv")))
	for length := 8; length <= len(hash); length += 4 {
		backend.addFile(&backend.views[0], "Movie.mkv-"+hash[:length]+".strm", []byte("strm"))
	}
	backend.addFile(&backend.views[0], "Movie.mkv-"+hash+"-1.strm", []byte("strm"))
	view := NewSTRMBackend(backend).Torrents()[0]
	if want := "Movie.mkv-" + hash + "-2.strm"; view.Files[0].Path != want {
		t.Fatalf("fully colliding name = %q, want %q", view.Files[0].Path, want)
	}
	if got := len(childrenOf(view.Files, "")); got != len(backend.views[0].Files) {
		t.Fatalf("visible entries = %d, want all %d", got, len(backend.views[0].Files))
	}
}

func TestSTRMNamesFitUTF8ByteLimitAndKeepEndings(t *testing.T) {
	backend := &strmTestBackend{fakeBackend: &fakeBackend{views: []TorrentView{{Name: "long", Hash: hashN(1)}}}}
	for _, path := range []string{
		strings.Repeat("中", 100) + ".mkv",
		strings.Repeat("中", 99) + "文.mkv",
		strings.Repeat("中", 100) + ".ISO",
		strings.Repeat("a", 251) + ".mkv",
		"中文/" + strings.Repeat("片", 100) + ".mp4",
	} {
		backend.addFile(&backend.views[0], path, []byte("video"))
	}
	projection := NewSTRMBackend(backend)
	view := projection.Torrents()[0]
	seen := make(map[string]bool)
	for i, file := range view.Files {
		parts := strings.Split(file.Path, "/")
		name := parts[len(parts)-1]
		if len(name) > 255 || !utf8.ValidString(name) || !strings.HasSuffix(name, ".strm") || !strings.Contains(name, "-") {
			t.Fatalf("invalid generated name %q (%d bytes)", name, len(name))
		}
		if i == 2 && !strings.HasSuffix(name, ".iso.strm") {
			t.Fatalf("long ISO lost its ending: %q", name)
		}
		if seen[file.Path] {
			t.Fatalf("long names collided at %q", file.Path)
		}
		seen[file.Path] = true
		reader, err := projection.OpenFile(view.Hash, file.Path)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := strmTestRead(t, reader), strmTestURL(view.Hash, backend.views[0].Files[i].Path)+"\n"; got != want || int64(len(got)) != file.Size {
			t.Fatalf("long path content/size mismatch: size=%d content=%q", file.Size, got)
		}
	}
	for _, name := range []string{strings.Repeat("中", 100) + ".iso", "../unsafe.mkv", "..\\unsafe.mkv", "bad\x00name.mkv"} {
		source := &strmTestBackend{fakeBackend: &fakeBackend{views: []TorrentView{{Name: name, Hash: hashN(2), SingleFile: true}}}}
		source.addFile(&source.views[0], "payload.mkv", []byte("video"))
		projected := NewSTRMBackend(source).Torrents()[0]
		if strings.ContainsAny(projected.Name, "/\\\x00") || len(projected.Name) > 255 || !utf8.ValidString(projected.Name) || !strings.HasSuffix(projected.Name, ".strm") {
			t.Fatalf("unsafe root name %q projected as %q", name, projected.Name)
		}
		if projected.Files[0].Path != "payload.mkv" {
			t.Fatalf("root identity changed to %q", projected.Files[0].Path)
		}
	}
}

func TestSTRMRootNamesRespectOriginalHashAndCategoryReservations(t *testing.T) {
	backend := &strmTestBackend{fakeBackend: &fakeBackend{
		categories: []string{"Category.strm", "Reserved.mkv", "empty"},
		views: []TorrentView{
			{Name: "Movie.mkv", Hash: hashN(1), SingleFile: true},
			{Name: "Movie.mkv", Hash: hashN(2), SingleFile: true},
			{Name: "Movie.mp4", Hash: hashN(3), SingleFile: true},
			{Name: "Movie.strm", Hash: hashN(4), SingleFile: true},
			{Name: "Folder.strm", Hash: hashN(5)},
			{Name: "Folder.mkv", Hash: hashN(6), SingleFile: true},
			{Name: "Disc.iso", Hash: hashN(7), SingleFile: true},
			{Name: "Disc.iso", Hash: hashN(8), SingleFile: true},
			{Name: "Category.mkv", Hash: hashN(9), SingleFile: true},
			{Name: "Reserved.mkv", Hash: hashN(10), SingleFile: true},
		},
	}}
	for i := range backend.views {
		path := backend.views[i].Name
		if i == 0 || i == 1 {
			path = fmt.Sprintf("original-%d.mkv", i)
		}
		if i == 4 {
			path = "readme.txt"
		}
		backend.addFile(&backend.views[i], path, []byte(path))
	}
	projection := NewSTRMBackend(backend)
	views := projection.Torrents()
	want := []string{
		"Movie.mkv.strm", "Movie-02000000.strm", "Movie.mp4.strm", "Movie.strm", "Folder.strm",
		"Folder.mkv.strm", "Disc.iso.strm", "Disc-08000000.iso.strm", "Category.mkv.strm", "Reserved-0a000000.strm",
	}
	for i, view := range views {
		if view.Name != want[i] {
			t.Errorf("root %d = %q, want %q", i, view.Name, want[i])
		}
		if got, ok := RootNameForWithCategories(view, views, backend.categories); !ok || got != view.Name {
			t.Errorf("root layout changed projected name %q to %q/%v", view.Name, got, ok)
		}
		if view.SingleFile && view.Files[0].Path != backend.views[i].Files[0].Path {
			t.Errorf("single-file identity changed from %q to %q", backend.views[i].Files[0].Path, view.Files[0].Path)
		}
	}
	originalName, ok := RootNameForWithCategories(backend.views[1], backend.views, backend.categories)
	if !ok || originalName != "Movie.mkv-02000000" {
		t.Fatalf("original duplicate root = %q/%v", originalName, ok)
	}
}

func TestSTRMOldSingleFileInodeSurvivesRootNameAndCategoryChanges(t *testing.T) {
	ctx := context.Background()
	backend := &strmTestBackend{fakeBackend: &fakeBackend{views: []TorrentView{{Name: "Movie.mkv", Hash: hashN(2), SingleFile: true}}}}
	backend.addFile(&backend.views[0], "original.mkv", []byte("video"))
	projection := NewSTRMBackend(backend)
	root := &rootNode{state: newFSState(projection)}
	_ = fs.NewNodeFS(root, nil)
	inode, errno := root.Lookup(ctx, "Movie.strm", &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("initial Lookup errno = %v", errno)
	}
	node := inode.Operations().(*torrentFileNode)
	if node.path != "original.mkv" {
		t.Fatalf("inode identity = %q, want original.mkv", node.path)
	}
	duplicate := TorrentView{Name: "Movie.mkv", Hash: hashN(1), SingleFile: true}
	backend.addFile(&duplicate, "duplicate.mkv", []byte("other"))
	backend.setViews(append(strmTestClone(backend.views), duplicate))
	if got := projection.Torrents()[0].Name; got != "Movie-02000000.strm" {
		t.Fatalf("name after adding a duplicate = %q", got)
	}
	assertRead := func() {
		t.Helper()
		handle, _, errno := node.Open(ctx, syscall.O_RDONLY)
		if errno != 0 {
			t.Fatalf("old inode Open errno = %v", errno)
		}
		want := strmTestURL(hashN(2), "original.mkv") + "\n"
		if got := strmTestRead(t, handle.(*readHandle).ra); got != want {
			t.Fatalf("old inode content = %q, want %q", got, want)
		}
	}
	assertRead()
	changed := strmTestClone(backend.views[:1])
	changed[0].Category = "movies"
	backend.categories = []string{"movies", "empty"}
	backend.setViews(changed)
	if got := projection.Torrents()[0].Name; got != "Movie.strm" {
		t.Fatalf("name after category change = %q, want Movie.strm", got)
	}
	assertRead()
	if _, errno := root.Lookup(ctx, "Movie.strm", &fuse.EntryOut{}); errno != syscall.ENOENT {
		t.Fatalf("moved root Lookup errno = %v, want ENOENT", errno)
	}
	if _, payload := backend.counts(); payload != 0 {
		t.Fatalf("generated inode opened payload %d times", payload)
	}
}

func TestSTRMNodesKeepReadOnlyAttributesAndInMemoryContent(t *testing.T) {
	ctx := context.Background()
	created := time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC)
	backend := &strmTestBackend{fakeBackend: &fakeBackend{views: []TorrentView{{Name: "Movie.mkv", Hash: hashN(1), CreatedAt: created, SingleFile: true}}}}
	backend.addFile(&backend.views[0], "Movie.mkv", []byte("must not be opened"))
	projection := NewSTRMBackend(backend)
	root := &rootNode{state: newFSState(projection)}
	_ = fs.NewNodeFS(root, nil)
	stream, errno := root.Readdir(ctx)
	if errno != 0 {
		t.Fatalf("Readdir errno = %v", errno)
	}
	defer stream.Close()
	entry, errno := stream.Next()
	if errno != 0 || entry.Name != "Movie.strm" || entry.Mode != syscall.S_IFREG || stream.HasNext() {
		t.Fatalf("Readdir entry = %+v/%v", entry, errno)
	}
	var out fuse.EntryOut
	inode, errno := root.Lookup(ctx, "Movie.strm", &out)
	if errno != 0 {
		t.Fatalf("Lookup errno = %v", errno)
	}
	want := strmTestURL(hashN(1), "Movie.mkv") + "\n"
	if out.Size != uint64(len(want)) || out.Mode&0o7777 != 0o444 {
		t.Fatalf("Lookup attr = %+v, want size %d and 0444", out.Attr, len(want))
	}
	assertAttrTimes(t, &out.Attr, created)
	node := inode.Operations().(*torrentFileNode)
	var attr fuse.AttrOut
	if errno := node.Getattr(ctx, nil, &attr); errno != 0 || attr.Size != uint64(len(want)) || attr.Nlink != 1 || attr.Mode&0o7777 != 0o444 {
		t.Fatalf("Getattr = %+v/%v", attr.Attr, errno)
	}
	for _, flags := range []uint32{syscall.O_WRONLY, syscall.O_RDWR, syscall.O_TRUNC} {
		if _, _, errno := node.Open(ctx, flags); errno != syscall.EROFS {
			t.Errorf("Open flags %d errno = %v, want EROFS", flags, errno)
		}
	}
	if errno := root.Unlink(ctx, "Movie.strm"); errno != syscall.EROFS {
		t.Fatalf("Unlink errno = %v, want EROFS", errno)
	}
	if _, _, _, errno := root.Create(ctx, "new", 0, 0, &fuse.EntryOut{}); errno != syscall.EROFS {
		t.Fatalf("Create errno = %v, want EROFS", errno)
	}
	handle, _, errno := node.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open errno = %v", errno)
	}
	read := handle.(*readHandle)
	buf := make([]byte, len(want)+10)
	result, errno := read.Read(ctx, buf, 0)
	if errno != 0 {
		t.Fatalf("Read errno = %v", errno)
	}
	got, status := result.Bytes(buf)
	if status != fuse.OK || string(got) != want || strings.Count(string(got), "\n") != 1 {
		t.Fatalf("STRM read = %q/%v, want one URL and one newline", got, status)
	}
	result, errno = read.Read(ctx, buf, int64(len(want)))
	if errno != 0 {
		t.Fatalf("EOF Read errno = %v", errno)
	}
	got, _ = result.Bytes(buf)
	if len(got) != 0 {
		t.Fatalf("EOF read = %q, want empty", got)
	}
	if errno := read.Release(ctx); errno != 0 {
		t.Fatalf("Release errno = %v", errno)
	}
	if _, payload := backend.counts(); payload != 0 {
		t.Fatalf("listing/stat/open/read touched payload %d times", payload)
	}
}

func TestSTRMCategoriesAndDynamicSubtitleSnapshots(t *testing.T) {
	ctx := context.Background()
	backend := &strmTestBackend{fakeBackend: &fakeBackend{
		categories: []string{"movies", "empty"},
		views:      []TorrentView{{Name: "Movie.mkv", Hash: hashN(1), Category: "movies", SingleFile: true}},
	}}
	backend.addFile(&backend.views[0], "Movie.mkv", []byte("video"))
	backend.addSubtitle(&backend.views[0], "Movie.srt", "Movie.mkv", []byte("old subtitle\n"))
	projection := NewSTRMBackend(backend)
	root := &rootNode{state: newFSState(projection)}
	_ = fs.NewNodeFS(root, nil)
	if entries := root.visibleChildren(); len(entries) != 2 || entries[0].Name != "empty" || entries[1].Name != "movies" {
		t.Fatalf("root entries = %+v, want empty/movies", entries)
	}
	emptyInode, errno := root.Lookup(ctx, "empty", &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("empty category Lookup errno = %v", errno)
	}
	empty := emptyInode.Operations().(*categoryNode)
	if entries := empty.visibleChildren(); len(entries) != 0 {
		t.Fatalf("empty category entries = %+v", entries)
	}
	movieInode, errno := root.Lookup(ctx, "movies", &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("movies Lookup errno = %v", errno)
	}
	movies := movieInode.Operations().(*categoryNode)
	if entries := movies.visibleChildren(); len(entries) != 2 || entries[0].Name != "Movie.srt" || entries[1].Name != "Movie.strm" {
		t.Fatalf("movie category entries = %+v", entries)
	}
	subtitleInode, errno := movies.Lookup(ctx, "Movie.srt", &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("subtitle Lookup errno = %v", errno)
	}
	node := subtitleInode.Operations().(*torrentFileNode)
	oldHandle, oldFlags, errno := node.Open(ctx, syscall.O_RDONLY)
	if errno != 0 || oldFlags != fuse.FOPEN_DIRECT_IO {
		t.Fatalf("old subtitle Open flags=%d errno=%v", oldFlags, errno)
	}
	replacement := "a much longer replacement subtitle\n"
	modified := time.Date(2026, 10, 3, 4, 5, 6, 0, time.UTC)
	backend.replaceSubtitle(hashN(1), "Movie.srt", []byte(replacement), modified)
	var attr fuse.AttrOut
	if errno := node.Getattr(ctx, nil, &attr); errno != 0 || attr.Size != uint64(len(replacement)) {
		t.Fatalf("replaced subtitle attr = %+v/%v", attr.Attr, errno)
	}
	if got := time.Unix(int64(attr.Mtime), int64(attr.Mtimensec)); !got.Equal(modified) {
		t.Fatalf("subtitle mtime = %s, want %s", got, modified)
	}
	newHandle, flags, errno := node.Open(ctx, syscall.O_RDONLY)
	if errno != 0 || flags != fuse.FOPEN_DIRECT_IO {
		t.Fatalf("new subtitle Open flags=%d errno=%v", flags, errno)
	}
	if got := strmTestRead(t, newHandle.(*readHandle).ra); got != replacement || newHandle.(*readHandle).size != int64(len(replacement)) {
		t.Fatalf("replacement content/size = %q/%d", got, newHandle.(*readHandle).size)
	}
	if got := strmTestRead(t, oldHandle.(*readHandle).ra); got != "old subtitle\n" {
		t.Fatalf("old subtitle snapshot = %q", got)
	}
	if _, payload := backend.counts(); payload != 0 {
		t.Fatalf("managed subtitle access opened payload %d times", payload)
	}
}

// strmBareBackend hides every optional capability of the shared fixture.
type strmBareBackend struct{ source *strmTestBackend }

func (b strmBareBackend) Torrents() []TorrentView { return b.source.Torrents() }
func (b strmBareBackend) OpenFile(hash metainfo.Hash, path string) (io.ReaderAt, error) {
	return b.source.OpenFile(hash, path)
}
func (b strmBareBackend) StreamURL(hash metainfo.Hash, path string) (string, error) {
	return b.source.StreamURL(hash, path)
}

func strmTestCapabilities(source *strmTestBackend, mask int) StreamURLBackend {
	bare := strmBareBackend{source: source}
	switch mask {
	case 1:
		return &struct {
			StreamURLBackend
			CategoryBackend
		}{bare, source}
	case 2:
		return &struct {
			StreamURLBackend
			SubtitleBackend
		}{bare, source}
	case 3:
		return &struct {
			StreamURLBackend
			CategoryBackend
			SubtitleBackend
		}{bare, source, source}
	case 4:
		return &struct {
			StreamURLBackend
			SubtitleStatBackend
		}{bare, source}
	case 5:
		return &struct {
			StreamURLBackend
			CategoryBackend
			SubtitleStatBackend
		}{bare, source, source}
	case 6:
		return &struct {
			StreamURLBackend
			SubtitleBackend
			SubtitleStatBackend
		}{bare, source, source}
	case 7:
		return &struct {
			StreamURLBackend
			CategoryBackend
			SubtitleBackend
			SubtitleStatBackend
		}{bare, source, source, source}
	default:
		return bare
	}
}

func TestSTRMOptionalCapabilitiesPreserveFallbacks(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		t.Run(fmt.Sprintf("capabilities-%d", mask), func(t *testing.T) {
			backend := &strmTestBackend{fakeBackend: &fakeBackend{
				categories: []string{"empty"},
				views:      []TorrentView{{Name: "Movie.mkv", Hash: hashN(1), SingleFile: true}},
			}}
			backend.addFile(&backend.views[0], "Movie.mkv", []byte("video"))
			backend.addSubtitle(&backend.views[0], "Movie.srt", "Movie.mkv", []byte("old"))
			projection := NewSTRMBackend(strmTestCapabilities(backend, mask))
			_, categories := projection.(CategoryBackend)
			_, subtitles := projection.(SubtitleBackend)
			_, stat := projection.(SubtitleStatBackend)
			if categories != (mask&1 != 0) || subtitles != (mask&2 != 0) || stat != (mask&4 != 0) {
				t.Fatalf("optional capabilities = %v/%v/%v for mask %d", categories, subtitles, stat, mask)
			}
			root := &rootNode{state: newFSState(projection)}
			_ = fs.NewNodeFS(root, nil)
			if _, errno := root.Lookup(context.Background(), "empty", &fuse.EntryOut{}); (errno == 0) != categories {
				t.Fatalf("empty category errno = %v with capability=%v", errno, categories)
			}
			inode, errno := root.Lookup(context.Background(), "Movie.srt", &fuse.EntryOut{})
			if errno != 0 {
				t.Fatalf("subtitle Lookup errno = %v", errno)
			}
			node := inode.Operations().(*torrentFileNode)
			backend.replaceSubtitle(hashN(1), "Movie.srt", []byte("a replacement"), time.Now().UTC())
			var attr fuse.AttrOut
			if errno := node.Getattr(context.Background(), nil, &attr); errno != 0 {
				t.Fatalf("subtitle Getattr errno = %v", errno)
			}
			wantSize := uint64(3)
			if stat {
				wantSize = uint64(len("a replacement"))
			}
			if attr.Size != wantSize {
				t.Fatalf("subtitle size = %d, want %d", attr.Size, wantSize)
			}
			_, _, errno = node.Open(context.Background(), syscall.O_RDONLY)
			if (errno == 0) != subtitles || (!subtitles && errno != syscall.ENOENT) {
				t.Fatalf("subtitle Open errno = %v with capability=%v", errno, subtitles)
			}
		})
	}
}

func TestSTRMDeletionAndSnapshotRaces(t *testing.T) {
	newBackend := func() *strmTestBackend {
		backend := &strmTestBackend{fakeBackend: &fakeBackend{views: []TorrentView{
			{Name: "deleted", Hash: hashN(1)}, {Name: "kept.mkv", Hash: hashN(2), SingleFile: true},
		}}}
		backend.addFile(&backend.views[0], "Movie.mkv", []byte("video"))
		backend.addFile(&backend.views[0], "Other.mp4", []byte("video"))
		backend.addFile(&backend.views[1], "kept.mkv", []byte("video"))
		return backend
	}
	t.Run("deleted-between-listing-and-open", func(t *testing.T) {
		backend := newBackend()
		projection := NewSTRMBackend(backend)
		view := projection.Torrents()[0]
		backend.setViews(strmTestClone(backend.views[1:]))
		if _, err := projection.OpenFile(view.Hash, view.Files[0].Path); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted torrent OpenFile = %v, want ErrNotFound", err)
		}
		if _, payload := backend.counts(); payload != 0 {
			t.Fatalf("deleted generated open reached payload %d times", payload)
		}
	})
	t.Run("deleted-between-open-snapshot-and-url", func(t *testing.T) {
		backend := newBackend()
		projection := NewSTRMBackend(backend)
		view := projection.Torrents()[0]
		backend.urlHook = func(metainfo.Hash, string) error {
			backend.setViews(strmTestClone(backend.views[1:]))
			return nil
		}
		if _, err := projection.OpenFile(view.Hash, view.Files[0].Path); !errors.Is(err, ErrNotFound) {
			t.Fatalf("racing deletion OpenFile = %v, want ErrNotFound", err)
		}
	})
	t.Run("deleted-during-listing", func(t *testing.T) {
		backend := newBackend()
		backend.urlHook = func(metainfo.Hash, string) error {
			backend.setViews(strmTestClone(backend.views[1:]))
			return nil
		}
		projection := NewSTRMBackend(backend)
		views := strmTestList(t, projection)
		if len(views) != 2 || len(views[0].Files) != 2 || !views[1].SingleFile || len(views[1].Files) != 1 || views[1].Name != "kept.strm" {
			t.Fatalf("racing listing lost snapshot entries: %+v", views)
		}
		for _, file := range views[0].Files {
			if file.Size != 0 || !strings.HasSuffix(file.Path, ".strm") {
				t.Fatalf("deleted file = %+v, want zero-sized generated identity", file)
			}
			if reader, err := projection.OpenFile(views[0].Hash, file.Path); reader != nil || !errors.Is(err, ErrNotFound) {
				t.Fatalf("deleted snapshot OpenFile = %T/%v, want nil/ErrNotFound", reader, err)
			}
		}
		file := views[1].Files[0]
		reader, err := projection.OpenFile(views[1].Hash, file.Path)
		if err != nil {
			t.Fatalf("surviving OpenFile: %v", err)
		}
		want := strmTestURL(hashN(2), "kept.mkv") + "\n"
		if got := strmTestRead(t, reader); got != want || file.Size != int64(len(want)) {
			t.Fatalf("surviving file content/size = %q/%d, want %q/%d", got, file.Size, want, len(want))
		}
		if _, payload := backend.counts(); payload != 0 {
			t.Fatalf("racing listing/open touched payload %d times", payload)
		}
		if next := strmTestList(t, projection); len(next) != 1 || next[0].Hash != hashN(2) {
			t.Fatalf("next listing retained the deleted torrent: %+v", next)
		}
	})
	t.Run("transient-url-error-does-not-hide-files", func(t *testing.T) {
		backend := newBackend()
		backend.urlHook = func(metainfo.Hash, string) error { return errors.New("snapshot changed") }
		projection := NewSTRMBackend(backend)
		views := strmTestList(t, projection)
		if len(views) != 2 || len(views[0].Files) != 2 || len(views[1].Files) != 1 {
			t.Fatalf("transient URL failure dropped files: %+v", views)
		}
		if file := views[0].Files[0]; file.Path != "Movie.strm" || file.Size != 0 {
			t.Fatalf("unavailable file = %+v, want zero-sized Movie.strm", file)
		}
		if stream, payload := backend.counts(); stream != 3 || payload != 0 {
			t.Fatalf("listing calls = %d URL/%d payload, want one URL attempt per file and no payload", stream, payload)
		}
		for i, view := range strmTestList(t, projection) {
			for j, file := range view.Files {
				want := strmTestURL(view.Hash, backend.views[i].Files[j].Path) + "\n"
				if file.Size != int64(len(want)) {
					t.Fatalf("size on next listing = %d, want %d", file.Size, len(want))
				}
			}
		}
	})
	t.Run("open-url-error-is-not-a-payload-fallback", func(t *testing.T) {
		backend := newBackend()
		projection := NewSTRMBackend(backend)
		view := projection.Torrents()[0]
		want := errors.New("not ready")
		backend.urlHook = func(metainfo.Hash, string) error { return want }
		if _, err := projection.OpenFile(view.Hash, view.Files[0].Path); !errors.Is(err, want) {
			t.Fatalf("OpenFile error = %v, want %v", err, want)
		}
		if _, payload := backend.counts(); payload != 0 {
			t.Fatalf("failed generated open used payload %d times", payload)
		}
	})
}

func TestSTRMPersistentURLErrorKeepsUnavailableFileAndHealthyNeighbors(t *testing.T) {
	for _, single := range []bool{false, true} {
		t.Run(fmt.Sprintf("single-file-%v", single), func(t *testing.T) {
			backend := &strmTestBackend{fakeBackend: &fakeBackend{views: []TorrentView{
				{Name: "bad.mkv", Hash: hashN(1), SingleFile: single},
				{Name: "healthy", Hash: hashN(2)},
			}}}
			const invalidPath = "bad\\name.mkv"
			backend.addFile(&backend.views[0], invalidPath, []byte("must never be opened"))
			for _, path := range []string{"Movie.mp4", "readme.txt", "existing.strm"} {
				backend.addFile(&backend.views[1], path, []byte("payload:"+path))
			}
			wantErr := fmt.Errorf("unsupported original path: %w", ErrInvalidName)
			backend.streamErrors = map[string]error{hashN(1).HexString() + "\x00" + invalidPath: wantErr}
			projection := NewSTRMBackend(backend)
			for attempt := 1; attempt <= 3; attempt++ {
				views := strmTestList(t, projection)
				if len(views) != 2 || len(views[0].Files) != 1 || views[0].SingleFile != single || len(views[1].Files) != 3 {
					t.Fatalf("persistent error changed snapshot structure: %+v", views)
				}
				bad := views[0].Files[0]
				wantPath := "bad_name.strm"
				if single {
					wantPath = invalidPath
					if views[0].Name != "bad.strm" {
						t.Fatalf("unavailable root name = %q, want bad.strm", views[0].Name)
					}
				}
				if bad.Path != wantPath || bad.Size != 0 {
					t.Fatalf("unavailable file = %+v, want %q with size zero", bad, wantPath)
				}
				if reader, err := projection.OpenFile(hashN(1), bad.Path); reader != nil || !errors.Is(err, wantErr) {
					t.Fatalf("unavailable OpenFile = %T/%v, want nil/original URL error", reader, err)
				}
				good := views[1].Files[0]
				reader, err := projection.OpenFile(hashN(2), good.Path)
				if err != nil {
					t.Fatalf("healthy OpenFile: %v", err)
				}
				want := strmTestURL(hashN(2), "Movie.mp4") + "\n"
				if got := strmTestRead(t, reader); got != want || good.Path != "Movie.strm" || good.Size != int64(len(want)) {
					t.Fatalf("healthy generated file = %+v/content %q, want correct STRM", good, got)
				}
				// The first listing signs both videos. Later listings reuse the
				// healthy video's text size and retry only the unavailable one,
				// so each attempt adds one listing retry and the two opens.
				if stream, payload := backend.counts(); stream != 4+(attempt-1)*3 || payload != 0 {
					t.Fatalf("calls = %d URL/%d payload, want bounded listing and no payload reads", stream, payload)
				}
			}
			root := &rootNode{state: newFSState(projection)}
			_ = fs.NewNodeFS(root, nil)
			rootName := "bad.mkv"
			if single {
				rootName = "bad.strm"
			}
			inode, errno := root.Lookup(context.Background(), rootName, &fuse.EntryOut{})
			if !single {
				if errno != 0 {
					t.Fatalf("torrent directory Lookup errno = %v", errno)
				}
				dir := inode.Operations().(*torrentDirNode)
				inode, errno = dir.Lookup(context.Background(), "bad_name.strm", &fuse.EntryOut{})
			}
			if inode != nil || errno != syscall.EINVAL {
				t.Fatalf("unavailable generated Lookup = %T/%v, want nil/EINVAL", inode, errno)
			}
			if _, payload := backend.counts(); payload != 0 {
				t.Fatalf("unavailable lookup opened payload %d times", payload)
			}
			for _, path := range []string{"readme.txt", "existing.strm"} {
				reader, err := projection.OpenFile(hashN(2), path)
				if err != nil {
					t.Fatalf("healthy passthrough %q: %v", path, err)
				}
				if got := strmTestRead(t, reader); got != "payload:"+path {
					t.Fatalf("healthy passthrough %q content = %q", path, got)
				}
			}
			backend.mu.Lock()
			backend.streamErrors = nil
			backend.mu.Unlock()
			views := strmTestList(t, projection)
			bad := views[0].Files[0]
			reader, err := projection.OpenFile(hashN(1), bad.Path)
			if err != nil {
				t.Fatalf("recovered OpenFile: %v", err)
			}
			want := strmTestURL(hashN(1), invalidPath) + "\n"
			if got := strmTestRead(t, reader); got != want || bad.Size != int64(len(want)) {
				t.Fatalf("recovered content/size = %q/%d, want %q/%d", got, bad.Size, want, len(want))
			}
		})
	}
}

func TestSTRMConcurrentListingsKeepImmutablePayloadMappings(t *testing.T) {
	backend := &strmTestBackend{fakeBackend: &fakeBackend{views: []TorrentView{
		{Name: "stable", Hash: hashN(1)}, {Name: "other.mkv", Hash: hashN(2), SingleFile: true},
	}}}
	for _, path := range []string{"nested/Movie.mkv", "nested/Movie.mp4", "nested/Movie.strm"} {
		backend.addFile(&backend.views[0], path, []byte("payload"))
	}
	backend.addFile(&backend.views[1], "other.mkv", []byte("payload"))
	original := strmTestClone(backend.views)
	projection := NewSTRMBackend(backend)
	initial := projection.Torrents()[0]
	wantPaths := strmTestPaths(original[0], initial)
	errs := make(chan error, 5)
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; i < 150; i++ {
			views := strmTestClone(original)
			if i%2 == 0 {
				views = views[:1]
				views[0].Category = "movies"
				views[0].Name = "renamed"
				// Even a dynamic subtitle directory named like a generated file
				// must not change the immutable payload's open mapping.
				views[0].Subtitles = []SubtitleView{{Path: "nested/Movie.mkv.strm/side.srt"}}
			}
			backend.mu.Lock()
			backend.views = views
			backend.categories = []string{"empty", "movies"}
			backend.mu.Unlock()
		}
	})
	for reader := 0; reader < 4; reader++ {
		wg.Go(func() {
			for i := 0; i < 75; i++ {
				views := projection.Torrents()
				var stable *TorrentView
				for j := range views {
					if views[j].Hash == hashN(1) {
						stable = &views[j]
						break
					}
				}
				if stable == nil || !reflect.DeepEqual(strmTestPaths(original[0], *stable), wantPaths) {
					errs <- fmt.Errorf("stable payload disappeared or changed mapping: %+v", stable)
					return
				}
				for j, file := range initial.Files[:2] {
					opened, err := projection.OpenFile(hashN(1), file.Path)
					if err != nil {
						errs <- fmt.Errorf("old inode path %q: %w", file.Path, err)
						return
					}
					want := strmTestURL(hashN(1), original[0].Files[j].Path) + "\n"
					buf := make([]byte, len(want))
					n, err := opened.ReadAt(buf, 0)
					if (err != nil && err != io.EOF) || n != len(want) || string(buf) != want {
						errs <- fmt.Errorf("old inode path %q read %q/%v", file.Path, buf, err)
						return
					}
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if _, payload := backend.counts(); payload != 0 {
		t.Fatalf("concurrent listings/STRM opens touched payload %d times", payload)
	}
}

// Every FUSE lookup lists the whole library, so a projection must not rename
// torrents or sign videos again there, and a lookup below a torrent must not
// take a library snapshot at all: a generated file costs one signature check.
func TestSTRMProjectionCachesTorrentsAndAvoidsSnapshotsOnFileLookups(t *testing.T) {
	created := time.Date(2026, 10, 20, 1, 2, 3, 0, time.UTC)
	backend := &strmTestBackend{fakeBackend: &fakeBackend{views: []TorrentView{
		{Name: "Collection", Hash: hashN(1), CreatedAt: created},
		{Name: "Single.mp4", Hash: hashN(2), SingleFile: true, CreatedAt: created},
	}}}
	for _, path := range []string{"Season 1/E01.mkv", "Season 1/E02.mkv", "Disc.iso", "notes.txt"} {
		backend.addFile(&backend.views[0], path, []byte("payload:"+path))
	}
	backend.addFile(&backend.views[1], "Single.mp4", []byte("single payload"))
	projection := NewSTRMBackend(backend)
	stat := projection.(FileStatBackend)

	first := strmTestList(t, projection)
	if stream, _ := backend.counts(); stream != 4 {
		t.Fatalf("first listing signed %d videos, want 4", stream)
	}
	for i := 0; i < 3; i++ {
		if again := strmTestList(t, projection); !reflect.DeepEqual(again, first) {
			t.Fatalf("cached listing = %+v, want %+v", again, first)
		}
	}
	if stream, _ := backend.counts(); stream != 4 {
		t.Fatalf("repeated listings signed %d times, want only the first four", stream)
	}
	paths := strmTestPaths(backend.views[0], first[0])
	if paths["Season 1/E01.mkv"] != "Season 1/E01.strm" || paths["Disc.iso"] != "Disc.iso.strm" || first[1].Name != "Single.strm" {
		t.Fatalf("projected names = %v / root %q", paths, first[1].Name)
	}

	snapshots := backend.snapshots()
	notes, err := stat.FileStat(hashN(1), paths["notes.txt"])
	if err != nil || notes.Size != int64(len("payload:notes.txt")) || !notes.CreatedAt.Equal(created) {
		t.Fatalf("passthrough FileStat = %+v/%v", notes, err)
	}
	if stream, _ := backend.counts(); stream != 4 {
		t.Fatalf("passthrough FileStat signed %d times", stream-4)
	}
	for _, original := range []string{"Season 1/E01.mkv", "Single.mp4"} {
		hash, path := hashN(1), paths[original]
		if original == "Single.mp4" {
			hash, path = hashN(2), original
		}
		want := strmTestURL(hash, original) + "\n"
		got, err := stat.FileStat(hash, path)
		if err != nil || got.Size != int64(len(want)) || !got.CreatedAt.Equal(created) {
			t.Fatalf("generated FileStat %q = %+v/%v, want size %d", path, got, err, len(want))
		}
	}
	if got := backend.snapshots(); got != snapshots {
		t.Fatalf("file lookups took %d library snapshots, want none", got-snapshots)
	}
	if stream, payload := backend.counts(); stream != 6 || payload != 0 {
		t.Fatalf("calls = %d URL/%d payload, want one signature per generated stat", stream, payload)
	}

	// A new added time (the torrent was removed and added again) is refreshed
	// by the next listing without renaming the torrent or signing it again.
	views := strmTestClone(backend.views)
	views[0].CreatedAt = created.Add(time.Hour)
	backend.setViews(views)
	strmTestList(t, projection)
	if got, err := stat.FileStat(hashN(1), paths["notes.txt"]); err != nil || !got.CreatedAt.Equal(created.Add(time.Hour)) {
		t.Fatalf("refreshed added time = %+v/%v", got, err)
	}
	if stream, _ := backend.counts(); stream != 6 {
		t.Fatalf("added-time refresh signed %d times", stream-6)
	}

	// A removed torrent's projection is dropped by the next listing; an inode
	// that outlives it costs one fresh snapshot and then fails closed.
	backend.setViews(strmTestClone(backend.views[:1]))
	strmTestList(t, projection)
	snapshots = backend.snapshots()
	if _, err := stat.FileStat(hashN(2), "Single.mp4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed torrent FileStat error = %v, want ErrNotFound", err)
	}
	if got := backend.snapshots(); got != snapshots+1 {
		t.Fatalf("removed torrent lookup took %d snapshots, want 1", got-snapshots)
	}

	// Changed payload metadata replaces the cached mapping on the next listing.
	views = strmTestClone(backend.views)
	backend.addFile(&views[0], "Season 1/E03.mkv", []byte("new episode"))
	backend.setViews(views)
	listed := strmTestList(t, projection)
	paths = strmTestPaths(views[0], listed[0])
	want := strmTestURL(hashN(1), "Season 1/E03.mkv") + "\n"
	if paths["Season 1/E03.mkv"] != "Season 1/E03.strm" || listed[0].Files[len(listed[0].Files)-1].Size != int64(len(want)) {
		t.Fatalf("changed payload listing = %+v", listed[0].Files)
	}
	reader, err := projection.OpenFile(hashN(1), "Season 1/E03.strm")
	if err != nil || strmTestRead(t, reader) != want {
		t.Fatalf("new episode STRM = %v", err)
	}
	if _, payload := backend.counts(); payload != 0 {
		t.Fatalf("cached projection opened payload %d times", payload)
	}
}

// Listings that prune and rebuild a torrent's cached projection race with
// stats and opens of both a stable torrent and the appearing one.
func TestSTRMCachedProjectionIsSafeForConcurrentUse(t *testing.T) {
	backend := &strmTestBackend{fakeBackend: &fakeBackend{views: []TorrentView{
		{Name: "stable", Hash: hashN(1)}, {Name: "flaky.mkv", Hash: hashN(2), SingleFile: true},
	}}}
	backend.addFile(&backend.views[0], "nested/Movie.mkv", []byte("payload"))
	backend.addFile(&backend.views[0], "nested/notes.txt", []byte("notes"))
	backend.addFile(&backend.views[1], "flaky.mkv", []byte("payload"))
	original := strmTestClone(backend.views)
	projection := NewSTRMBackend(backend)
	stat := projection.(FileStatBackend)
	snapshot := projection.(FileSnapshotBackend)
	stableWant := strmTestURL(hashN(1), "nested/Movie.mkv") + "\n"
	flakyWant := strmTestURL(hashN(2), "flaky.mkv") + "\n"
	errs := make(chan error, 8)
	report := func(err error) {
		select {
		case errs <- err:
		default:
		}
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; i < 200; i++ {
			views := strmTestClone(original)
			if i%2 == 0 {
				views = views[:1]
			}
			backend.setViews(views)
			projection.Torrents()
		}
	})
	for worker := 0; worker < 4; worker++ {
		wg.Go(func() {
			for i := 0; i < 150; i++ {
				if got, err := stat.FileStat(hashN(1), "nested/Movie.strm"); err != nil || got.Size != int64(len(stableWant)) {
					report(fmt.Errorf("stable FileStat = %+v/%v", got, err))
					return
				}
				if got, err := stat.FileStat(hashN(1), "nested/notes.txt"); err != nil || got.Size != int64(len("notes")) {
					report(fmt.Errorf("stable passthrough FileStat = %+v/%v", got, err))
					return
				}
				opened, err := snapshot.OpenFileSnapshot(hashN(1), "nested/Movie.strm")
				if err != nil || opened.Size != int64(len(stableWant)) {
					report(fmt.Errorf("stable OpenFileSnapshot = %+v/%v", opened, err))
					return
				}
				buf := make([]byte, len(stableWant))
				if n, err := opened.Reader.ReadAt(buf, 0); (err != nil && err != io.EOF) || n != len(stableWant) || string(buf) != stableWant {
					report(fmt.Errorf("stable STRM read %q/%v", buf, err))
					return
				}
				// The appearing torrent is either absent or exact, never empty.
				if got, err := stat.FileStat(hashN(2), "flaky.mkv"); err == nil && got.Size != int64(len(flakyWant)) {
					report(fmt.Errorf("flaky FileStat size = %d, want %d", got.Size, len(flakyWant)))
					return
				} else if err != nil && !errors.Is(err, ErrNotFound) {
					report(fmt.Errorf("flaky FileStat error = %v", err))
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	backend.setViews(strmTestClone(original))
	if views := strmTestList(t, projection); len(views) != 2 || views[1].Files[0].Size != int64(len(flakyWant)) {
		t.Fatalf("final listing = %+v", views)
	}
	if _, payload := backend.counts(); payload != 0 {
		t.Fatalf("concurrent STRM metadata touched payload %d times", payload)
	}
}
