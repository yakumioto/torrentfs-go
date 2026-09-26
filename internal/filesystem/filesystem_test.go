package filesystem

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type fakeBackend struct {
	views    []TorrentView
	data     map[string][]byte
	subtitle map[string][]byte
}

func (b *fakeBackend) Torrents() []TorrentView { return b.views }

func (b *fakeBackend) OpenFile(hash metainfo.Hash, path string) (io.ReaderAt, error) {
	data, ok := b.data[hash.HexString()+"\x00"+path]
	if !ok {
		return nil, fmt.Errorf("fake backend: no file %q", path)
	}
	return bytes.NewReader(data), nil
}

func (b *fakeBackend) OpenSubtitle(hash metainfo.Hash, path string) (io.ReaderAt, error) {
	data, ok := b.subtitle[hash.HexString()+"\x00"+path]
	if !ok {
		return nil, fmt.Errorf("fake backend: no subtitle %q", path)
	}
	return bytes.NewReader(data), nil
}

func (b *fakeBackend) addSubtitle(view *TorrentView, path, videoPath string, data []byte) {
	view.Subtitles = append(view.Subtitles, SubtitleView{Path: path, VideoPath: videoPath, Size: int64(len(data))})
	if b.subtitle == nil {
		b.subtitle = make(map[string][]byte)
	}
	b.subtitle[view.Hash.HexString()+"\x00"+path] = data
}

// replaceSubtitle swaps the stored bytes without touching the view snapshot, so
// a test can prove which source the node reads its metadata from.
func (b *fakeBackend) replaceSubtitle(hash metainfo.Hash, path string, data []byte) {
	b.subtitle[hash.HexString()+"\x00"+path] = data
}

func (b *fakeBackend) SubtitleStat(hash metainfo.Hash, path string) (SubtitleStat, error) {
	data, ok := b.subtitle[hash.HexString()+"\x00"+path]
	if !ok {
		return SubtitleStat{}, fmt.Errorf("fake backend: no subtitle %q", path)
	}
	return SubtitleStat{Size: int64(len(data))}, nil
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

func assertAttrTimes(t *testing.T, attr *fuse.Attr, want time.Time) {
	t.Helper()
	if want.IsZero() {
		if attr.Atime != 0 || attr.Mtime != 0 || attr.Ctime != 0 ||
			attr.Atimensec != 0 || attr.Mtimensec != 0 || attr.Ctimensec != 0 {
			t.Fatalf("zero CreatedAt produced non-zero attr times: %+v", *attr)
		}
		return
	}
	wantSec := uint64(want.Unix())
	wantNsec := uint32(want.Nanosecond())
	if attr.Atime != wantSec || attr.Mtime != wantSec || attr.Ctime != wantSec ||
		attr.Atimensec != wantNsec || attr.Mtimensec != wantNsec || attr.Ctimensec != wantNsec {
		t.Fatalf("attr times = %d.%09d/%d.%09d/%d.%09d, want %d.%09d",
			attr.Atime, attr.Atimensec, attr.Mtime, attr.Mtimensec, attr.Ctime, attr.Ctimensec,
			wantSec, wantNsec)
	}
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

func TestTorrentNodeTimesFollowCreatedAt(t *testing.T) {
	ctx := context.Background()
	createdAt := time.Date(2026, 9, 24, 6, 30, 4, 123456789, time.UTC)
	backend := &fakeBackend{views: []TorrentView{
		{Name: "single", Hash: hashN(1), SingleFile: true, CreatedAt: createdAt},
		{Name: "multi", Hash: hashN(2), CreatedAt: createdAt},
		{Name: "zero", Hash: hashN(3), SingleFile: true},
	}}
	backend.addFile(&backend.views[0], "payload.bin", []byte("single"))
	backend.addFile(&backend.views[1], "top.txt", []byte("top"))
	backend.addFile(&backend.views[1], "sub/deep/payload.bin", []byte("nested"))
	backend.addFile(&backend.views[2], "zero.bin", []byte("zero"))
	root := &rootNode{state: newFSState(backend)}
	_ = fs.NewNodeFS(root, nil)

	var singleOut fuse.EntryOut
	singleInode, errno := root.Lookup(ctx, "single", &singleOut)
	if errno != 0 {
		t.Fatalf("single root Lookup errno = %v", errno)
	}
	assertAttrTimes(t, &singleOut.Attr, createdAt)
	singleNode, ok := singleInode.Operations().(*torrentFileNode)
	if !ok {
		t.Fatalf("single node = %T, want *torrentFileNode", singleInode.Operations())
	}
	var singleAttr fuse.AttrOut
	if errno := singleNode.Getattr(ctx, nil, &singleAttr); errno != 0 {
		t.Fatalf("single Getattr errno = %v", errno)
	}
	assertAttrTimes(t, &singleAttr.Attr, createdAt)

	var multiOut fuse.EntryOut
	multiInode, errno := root.Lookup(ctx, "multi", &multiOut)
	if errno != 0 {
		t.Fatalf("multi root Lookup errno = %v", errno)
	}
	assertAttrTimes(t, &multiOut.Attr, createdAt)
	multiNode, ok := multiInode.Operations().(*torrentDirNode)
	if !ok {
		t.Fatalf("multi node = %T, want *torrentDirNode", multiInode.Operations())
	}
	var multiAttr fuse.AttrOut
	if errno := multiNode.Getattr(ctx, nil, &multiAttr); errno != 0 {
		t.Fatalf("multi Getattr errno = %v", errno)
	}
	assertAttrTimes(t, &multiAttr.Attr, createdAt)

	var subOut fuse.EntryOut
	subInode, errno := multiNode.Lookup(ctx, "sub", &subOut)
	if errno != 0 {
		t.Fatalf("sub Lookup errno = %v", errno)
	}
	assertAttrTimes(t, &subOut.Attr, createdAt)
	subNode, ok := subInode.Operations().(*torrentDirNode)
	if !ok {
		t.Fatalf("sub node = %T, want *torrentDirNode", subInode.Operations())
	}
	var subAttr fuse.AttrOut
	if errno := subNode.Getattr(ctx, nil, &subAttr); errno != 0 {
		t.Fatalf("sub Getattr errno = %v", errno)
	}
	assertAttrTimes(t, &subAttr.Attr, createdAt)

	var deepOut fuse.EntryOut
	deepInode, errno := subNode.Lookup(ctx, "deep", &deepOut)
	if errno != 0 {
		t.Fatalf("deep Lookup errno = %v", errno)
	}
	assertAttrTimes(t, &deepOut.Attr, createdAt)
	deepNode, ok := deepInode.Operations().(*torrentDirNode)
	if !ok {
		t.Fatalf("deep node = %T, want *torrentDirNode", deepInode.Operations())
	}
	var deepAttr fuse.AttrOut
	if errno := deepNode.Getattr(ctx, nil, &deepAttr); errno != 0 {
		t.Fatalf("deep Getattr errno = %v", errno)
	}
	assertAttrTimes(t, &deepAttr.Attr, createdAt)

	var leafOut fuse.EntryOut
	leafInode, errno := deepNode.Lookup(ctx, "payload.bin", &leafOut)
	if errno != 0 {
		t.Fatalf("leaf Lookup errno = %v", errno)
	}
	assertAttrTimes(t, &leafOut.Attr, createdAt)
	leafNode, ok := leafInode.Operations().(*torrentFileNode)
	if !ok {
		t.Fatalf("leaf node = %T, want *torrentFileNode", leafInode.Operations())
	}
	var leafAttr fuse.AttrOut
	if errno := leafNode.Getattr(ctx, nil, &leafAttr); errno != 0 {
		t.Fatalf("leaf Getattr errno = %v", errno)
	}
	assertAttrTimes(t, &leafAttr.Attr, createdAt)

	var zeroOut fuse.EntryOut
	zeroInode, errno := root.Lookup(ctx, "zero", &zeroOut)
	if errno != 0 {
		t.Fatalf("zero root Lookup errno = %v", errno)
	}
	assertAttrTimes(t, &zeroOut.Attr, time.Time{})
	zeroNode, ok := zeroInode.Operations().(*torrentFileNode)
	if !ok {
		t.Fatalf("zero node = %T, want *torrentFileNode", zeroInode.Operations())
	}
	var zeroAttr fuse.AttrOut
	if errno := zeroNode.Getattr(ctx, nil, &zeroAttr); errno != 0 {
		t.Fatalf("zero Getattr errno = %v", errno)
	}
	assertAttrTimes(t, &zeroAttr.Attr, time.Time{})
}

// contextualFakeReader implements both io.ReaderAt and the optional
// contextualReaderAt extension: its contextual read blocks until the request
// context is cancelled, and its plain ReadAt records that it was reached, so a
// test can prove which path the handle took.
type contextualFakeReader struct {
	started     chan struct{}
	startedOnce sync.Once
	plainCalls  int
	mu          sync.Mutex
}

func (r *contextualFakeReader) ReadAt([]byte, int64) (int, error) {
	r.mu.Lock()
	r.plainCalls++
	r.mu.Unlock()
	return 0, nil
}

func (r *contextualFakeReader) ReadAtContext(ctx context.Context, _ []byte, _ int64) (int, error) {
	r.startedOnce.Do(func() { close(r.started) })
	<-ctx.Done()
	return 0, ctx.Err()
}

func (r *contextualFakeReader) plainCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.plainCalls
}

// TestReadHandleForwardsRequestContext checks the FUSE bridge hands the
// request context to a context-aware reader: cancelling the request fails the
// read with EINTR instead of leaving it blocked, and the plain io.ReaderAt
// path is never used for such a reader.
func TestReadHandleForwardsRequestContext(t *testing.T) {
	reader := &contextualFakeReader{started: make(chan struct{})}
	handle := &readHandle{ra: reader, size: 4096}

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		errno syscall.Errno
		data  []byte
	}
	done := make(chan result, 1)
	go func() {
		read, errno := handle.Read(ctx, make([]byte, 64), 0)
		var data []byte
		if read != nil {
			data, _ = read.Bytes(make([]byte, 64))
		}
		done <- result{errno: errno, data: data}
	}()

	select {
	case <-reader.started:
	case <-time.After(5 * time.Second):
		t.Fatal("contextual read never started")
	}
	cancel()
	select {
	case got := <-done:
		if got.errno != syscall.EINTR {
			t.Fatalf("cancelled read errno = %v, want EINTR", got.errno)
		}
		if len(got.data) != 0 {
			t.Fatalf("cancelled read returned %d bytes of data, want none", len(got.data))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled read did not return")
	}
	if got := reader.plainCallCount(); got != 0 {
		t.Fatalf("plain ReadAt calls = %d, want 0 for a context-aware reader", got)
	}
}

// TestReadHandleFallsBackToPlainReaderAt pins the compatibility path: a
// backend that only implements io.ReaderAt still serves reads normally.
func TestReadHandleFallsBackToPlainReaderAt(t *testing.T) {
	handle := &readHandle{ra: bytes.NewReader([]byte("content")), size: 7}
	read, errno := handle.Read(context.Background(), make([]byte, 7), 0)
	if errno != 0 {
		t.Fatalf("plain read errno = %v, want 0", errno)
	}
	out, status := read.Bytes(make([]byte, 7))
	if status != fuse.OK || string(out) != "content" {
		t.Fatalf("plain read = %q status=%v, want content/OK", out, status)
	}
}

func TestSubtitleEntriesMergeIntoTheTorrentLayout(t *testing.T) {
	files := []FileView{
		{Path: "top.mkv", Size: 10},
		{Path: "Season 1/E01.mp4", Size: 20},
	}
	subtitles := []SubtitleView{
		{Path: "top.srt", VideoPath: "top.mkv", Size: 3},
		{Path: "Season 1/E01.srt", VideoPath: "Season 1/E01.mp4", Size: 4},
	}
	root := childrenOfViews(files, subtitles, "")
	if want := []string{"Season 1", "top.mkv", "top.srt"}; !reflect.DeepEqual(entryNames(root), want) {
		t.Fatalf("root children = %v, want %v", entryNames(root), want)
	}
	for _, entry := range root {
		if entry.Name == "top.srt" && (entry.IsDir || !entry.IsSubtitle) {
			t.Fatalf("top.srt entry = %+v, want a subtitle file", entry)
		}
	}
	nested := childrenOfViews(files, subtitles, "Season 1")
	if want := []string{"E01.mp4", "E01.srt"}; !reflect.DeepEqual(entryNames(nested), want) {
		t.Fatalf("nested children = %v, want %v", entryNames(nested), want)
	}
	if entry, ok := lookupChildWithSubtitles(files, subtitles, "Season 1", "E01.srt"); !ok || !entry.IsSubtitle {
		t.Fatalf("lookup E01.srt = %+v (%v), want a subtitle entry", entry, ok)
	}
	if _, ok := lookupChild(files, "Season 1", "E01.srt"); ok {
		t.Fatal("payload-only lookup found a managed subtitle")
	}
}

// TestSubtitleNeverShadowsPayload pins the precedence rule: a managed sidecar
// must not displace an immutable payload file that happens to share its path.
func TestSubtitleNeverShadowsPayload(t *testing.T) {
	files := []FileView{{Path: "movie.mkv", Size: 10}, {Path: "movie.srt", Size: 99}}
	subtitles := []SubtitleView{{Path: "movie.srt", VideoPath: "movie.mkv", Size: 3}}
	entries := childrenOfViews(files, subtitles, "")
	for _, entry := range entries {
		if entry.Name == "movie.srt" {
			if entry.IsSubtitle || entry.Size != 99 {
				t.Fatalf("movie.srt = %+v, want the payload file to win", entry)
			}
		}
	}
}

func TestSingleFileSubtitleAppearsBesideTheVideo(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{views: []TorrentView{
		{Name: "Movie.mkv", Hash: hashN(1), SingleFile: true},
	}}
	backend.addFile(&backend.views[0], "Movie.mkv", []byte("video"))
	backend.addSubtitle(&backend.views[0], "Movie.srt", "Movie.mkv", []byte("sub"))
	root := &rootNode{state: newFSState(backend)}
	_ = fs.NewNodeFS(root, nil)

	entries := root.visibleChildren()
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	if want := []string{"Movie.mkv", "Movie.srt"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("root children = %v, want %v", names, want)
	}

	var subtitleOut fuse.EntryOut
	subtitleInode, errno := root.Lookup(ctx, "Movie.srt", &subtitleOut)
	if errno != 0 {
		t.Fatalf("Lookup Movie.srt errno = %v", errno)
	}
	if subtitleOut.Mode&0o7777 != 0o444 {
		t.Fatalf("subtitle mode = %o, want 0444", subtitleOut.Mode)
	}
	node, ok := subtitleInode.Operations().(*torrentFileNode)
	if !ok {
		t.Fatalf("subtitle node = %T, want *torrentFileNode", subtitleInode.Operations())
	}
	if _, _, errno := node.Open(ctx, syscall.O_WRONLY); errno != syscall.EROFS {
		t.Fatalf("write Open on a subtitle errno = %v, want EROFS", errno)
	}
	handle, _, errno := node.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("read Open on a subtitle errno = %v", errno)
	}
	read, ok := handle.(*readHandle)
	if !ok {
		t.Fatalf("handle = %T, want *readHandle", handle)
	}
	buf := make([]byte, 3)
	result, errno := read.Read(ctx, buf, 0)
	if errno != 0 {
		t.Fatalf("Read errno = %v", errno)
	}
	out, status := result.Bytes(buf)
	if status != fuse.OK || string(out) != "sub" {
		t.Fatalf("subtitle read = %q status=%v, want sub/OK", out, status)
	}
	// Direct mutation through the mount stays impossible for subtitles too.
	if errno := root.Unlink(ctx, "Movie.srt"); errno != syscall.EROFS {
		t.Fatalf("Unlink Movie.srt errno = %v, want EROFS", errno)
	}
	if _, errno := root.Mkdir(ctx, "new", 0, &fuse.EntryOut{}); errno != syscall.EROFS {
		t.Fatalf("Mkdir errno = %v, want EROFS", errno)
	}
}

// TestSubtitleNodeReportsReplacedLength pins the metadata contract: the inode is
// cached by path, so after a replacement the same node must report the new
// length and open with the new read bound instead of truncating at the length
// the inode first saw.
func TestSubtitleNodeReportsReplacedLength(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{views: []TorrentView{{Name: "Movie.mkv", Hash: hashN(1), SingleFile: true}}}
	backend.addFile(&backend.views[0], "Movie.mkv", []byte("video"))
	backend.addSubtitle(&backend.views[0], "Movie.srt", "Movie.mkv", []byte("short"))
	root := &rootNode{state: newFSState(backend)}
	_ = fs.NewNodeFS(root, nil)

	var entryOut fuse.EntryOut
	inode, errno := root.Lookup(ctx, "Movie.srt", &entryOut)
	if errno != 0 {
		t.Fatalf("Lookup Movie.srt errno = %v", errno)
	}
	node, ok := inode.Operations().(*torrentFileNode)
	if !ok {
		t.Fatalf("node = %T, want *torrentFileNode", inode.Operations())
	}
	var attr fuse.AttrOut
	if errno := node.Getattr(ctx, nil, &attr); errno != 0 {
		t.Fatalf("Getattr errno = %v", errno)
	}
	if attr.Size != 5 {
		t.Fatalf("initial size = %d, want 5", attr.Size)
	}

	// A replacement changes the file's length; the view snapshot deliberately
	// keeps the old size, as a cached inode would.
	longer := bytes.Repeat([]byte("long-subtitle\n"), 512)
	backend.replaceSubtitle(hashN(1), "Movie.srt", longer)

	if errno := node.Getattr(ctx, nil, &attr); errno != 0 {
		t.Fatalf("Getattr after replace errno = %v", errno)
	}
	if attr.Size != uint64(len(longer)) {
		t.Fatalf("size after replace = %d, want %d", attr.Size, len(longer))
	}
	handle, _, errno := node.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open errno = %v", errno)
	}
	read, ok := handle.(*readHandle)
	if !ok {
		t.Fatalf("handle = %T, want *readHandle", handle)
	}
	if read.size != int64(len(longer)) {
		t.Fatalf("read bound = %d, want the replaced length %d", read.size, len(longer))
	}
	// The last page must come back whole: a bound fixed at the old length would
	// report the tail as out of range.
	tail := make([]byte, 16)
	result, errno := read.Read(ctx, tail, int64(len(longer))-16)
	if errno != 0 {
		t.Fatalf("tail Read errno = %v", errno)
	}
	out, status := result.Bytes(tail)
	if status != fuse.OK || string(out) != string(longer[len(longer)-16:]) {
		t.Fatalf("tail read = %q status=%v, want the replaced tail", out, status)
	}
}

// TestSubtitleOpenKeepsTheOldSnapshotForAnOpenHandle covers the other side of
// the same contract: a handle opened before a replacement keeps reading the
// bytes it opened, which is what the file's own inode still holds.
func TestSubtitleOpenKeepsTheOldSnapshotForAnOpenHandle(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{views: []TorrentView{{Name: "Movie.mkv", Hash: hashN(1), SingleFile: true}}}
	backend.addFile(&backend.views[0], "Movie.mkv", []byte("video"))
	backend.addSubtitle(&backend.views[0], "Movie.srt", "Movie.mkv", []byte("original\n"))
	root := &rootNode{state: newFSState(backend)}
	_ = fs.NewNodeFS(root, nil)

	var entryOut fuse.EntryOut
	inode, errno := root.Lookup(ctx, "Movie.srt", &entryOut)
	if errno != 0 {
		t.Fatalf("Lookup Movie.srt errno = %v", errno)
	}
	node := inode.Operations().(*torrentFileNode)
	handle, _, errno := node.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open errno = %v", errno)
	}
	read := handle.(*readHandle)

	backend.replaceSubtitle(hashN(1), "Movie.srt", []byte("a much longer replacement\n"))

	buf := make([]byte, 16)
	result, errno := read.Read(ctx, buf, 0)
	if errno != 0 {
		t.Fatalf("old handle read errno = %v", errno)
	}
	out, _ := result.Bytes(buf)
	if string(out) != "original\n" {
		t.Fatalf("old handle read = %q, want the snapshot it opened", out)
	}
}

// TestSubtitleReplacementIsServedByALaterOpen proves the kernel-facing
// guarantee: after a byte-for-byte replacement, a fresh open reads the new
// content, because nothing caches the old snapshot.
func TestSubtitleReplacementIsServedByALaterOpen(t *testing.T) {
	ctx := context.Background()
	backend := &fakeBackend{views: []TorrentView{{Name: "show", Hash: hashN(1)}}}
	backend.addFile(&backend.views[0], "movie.mkv", []byte("video"))
	backend.addSubtitle(&backend.views[0], "movie.srt", "movie.mkv", []byte("old"))
	root := &rootNode{state: newFSState(backend)}
	_ = fs.NewNodeFS(root, nil)

	var entryOut fuse.EntryOut
	dirInode, errno := root.Lookup(ctx, "show", &entryOut)
	if errno != 0 {
		t.Fatalf("Lookup show errno = %v", errno)
	}
	dir, ok := dirInode.Operations().(*torrentDirNode)
	if !ok {
		t.Fatalf("node = %T, want *torrentDirNode", dirInode.Operations())
	}
	var fileOut fuse.EntryOut
	fileInode, errno := dir.Lookup(ctx, "movie.srt", &fileOut)
	if errno != 0 {
		t.Fatalf("Lookup movie.srt errno = %v", errno)
	}
	node := fileInode.Operations().(*torrentFileNode)
	first, _, errno := node.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("first Open errno = %v", errno)
	}
	buf := make([]byte, 16)
	result, _ := first.(*readHandle).Read(ctx, buf, 0)
	out, _ := result.Bytes(buf)
	if string(out) != "old" {
		t.Fatalf("first read = %q, want old", out)
	}

	backend.subtitle[hashN(1).HexString()+"\x00movie.srt"] = []byte("new subtitle")
	second, _, errno := node.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("second Open errno = %v", errno)
	}
	result, _ = second.(*readHandle).Read(ctx, buf, 0)
	out, _ = result.Bytes(buf)
	if string(out) != "new subtitle" {
		t.Fatalf("second read = %q, want the replacement content", out)
	}
}

func TestRootNameForReportsTheDisambiguatedName(t *testing.T) {
	views := []TorrentView{{Name: "dup", Hash: hashN(1)}, {Name: "dup", Hash: hashN(2)}}
	firstName, ok := RootNameFor(views[0], views)
	if !ok || firstName != "dup" {
		t.Fatalf("first name = %q (%v), want dup", firstName, ok)
	}
	secondName, ok := RootNameFor(views[1], views)
	if !ok || secondName == "dup" || !strings.HasPrefix(secondName, "dup-") {
		t.Fatalf("second name = %q (%v), want a disambiguated name", secondName, ok)
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
