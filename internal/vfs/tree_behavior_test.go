package vfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

type treePayload struct {
	data    []byte
	created time.Time
}

type treeSubtitle struct {
	data     []byte
	modified time.Time
}

type treeFixture struct {
	mu         sync.Mutex
	views      []TorrentView
	categories []string
	payloads   map[string]treePayload
	subtitles  map[string]treeSubtitle
	readers    map[string]io.ReaderAt
	opens      atomic.Int64
}

func treeFixtureKey(hash metainfo.Hash, path string) string { return hash.HexString() + "\x00" + path }

func newTreeFixture(views ...TorrentView) *treeFixture {
	return &treeFixture{views: views, payloads: make(map[string]treePayload),
		subtitles: make(map[string]treeSubtitle), readers: make(map[string]io.ReaderAt)}
}

func cloneTreeViews(views []TorrentView) []TorrentView {
	out := slices.Clone(views)
	for i := range out {
		if out[i].FilesID == nil {
			out[i].Files = slices.Clone(out[i].Files)
		}
		out[i].Subtitles = slices.Clone(out[i].Subtitles)
	}
	return out
}

func (b *treeFixture) Torrents() []TorrentView {
	b.mu.Lock()
	defer b.mu.Unlock()
	return cloneTreeViews(b.views)
}

func (b *treeFixture) Categories() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.categories)
}

func (b *treeFixture) TorrentContents(hash metainfo.Hash) (TorrentContents, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, view := range b.views {
		if view.Hash == hash {
			return TorrentContents{Files: view.Files, FilesID: view.FilesID,
				Subtitles: slices.Clone(view.Subtitles), CreatedAt: view.CreatedAt, SingleFile: view.SingleFile}, nil
		}
	}
	return TorrentContents{}, ErrNotFound
}

func (b *treeFixture) OpenFile(hash metainfo.Hash, path string) (io.ReaderAt, error) {
	snapshot, err := b.OpenFileSnapshot(hash, path)
	return snapshot.Reader, err
}

func (b *treeFixture) OpenFileSnapshot(hash metainfo.Hash, path string) (FileSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := treeFixtureKey(hash, path)
	payload, ok := b.payloads[key]
	if !ok {
		return FileSnapshot{}, ErrNotFound
	}
	b.opens.Add(1)
	var reader io.ReaderAt = bytes.NewReader(payload.data)
	if custom := b.readers[key]; custom != nil {
		reader = custom
	}
	return FileSnapshot{Reader: reader, Size: int64(len(payload.data)), CreatedAt: payload.created}, nil
}

func (b *treeFixture) FileStat(hash metainfo.Hash, path string) (FileStat, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	payload, ok := b.payloads[treeFixtureKey(hash, path)]
	if !ok {
		return FileStat{}, ErrNotFound
	}
	return FileStat{Size: int64(len(payload.data)), CreatedAt: payload.created}, nil
}

func (b *treeFixture) OpenSubtitle(hash metainfo.Hash, path string) (SubtitleSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	subtitle, ok := b.subtitles[treeFixtureKey(hash, path)]
	if !ok {
		return SubtitleSnapshot{}, ErrNotFound
	}
	b.opens.Add(1)
	return SubtitleSnapshot{Reader: bytes.NewReader(subtitle.data), Size: int64(len(subtitle.data)), ModifiedAt: subtitle.modified}, nil
}

func (b *treeFixture) SubtitleStat(hash metainfo.Hash, path string) (SubtitleStat, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	subtitle, ok := b.subtitles[treeFixtureKey(hash, path)]
	if !ok {
		return SubtitleStat{}, ErrNotFound
	}
	return SubtitleStat{Size: int64(len(subtitle.data)), ModifiedAt: subtitle.modified}, nil
}

func treeEntryNames(entries []Entry) []string {
	out := make([]string, len(entries))
	for i := range entries {
		out[i] = entries[i].Name
	}
	return out
}

func readTreeHandle(t *testing.T, handle *Handle) string {
	t.Helper()
	buf := make([]byte, handle.Metadata().Size+8)
	n, err := handle.ReadAt(buf, 0)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("read past snapshot end: n=%d err=%v", n, err)
	}
	return string(buf[:n])
}

func closeTreeHandle(t *testing.T, handle *Handle) {
	t.Helper()
	if err := handle.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestTreeCategoriesSingleFilesAndNameCollisions(t *testing.T) {
	created := time.Unix(100, 0).UTC()
	b := newTreeFixture(
		TorrentView{Name: "Film.mkv", Hash: metainfo.Hash{1}, FilesID: &FileListID{}, SingleFile: true, CreatedAt: created, Files: []FileView{{Path: "original.mkv", Size: 4}}},
		TorrentView{Name: "Film.mkv", Hash: metainfo.Hash{2}, FilesID: &FileListID{}, SingleFile: true, CreatedAt: created, Files: []FileView{{Path: "other.mkv", Size: 5}}},
		TorrentView{Name: "Series", Hash: metainfo.Hash{3}, Category: "tv", FilesID: &FileListID{}, CreatedAt: created, Files: []FileView{{Path: "season/E01.mkv", Size: 6}}},
	)
	b.categories = []string{"empty", "tv"}
	b.payloads[treeFixtureKey(metainfo.Hash{1}, "original.mkv")] = treePayload{data: []byte("film"), created: created}
	b.payloads[treeFixtureKey(metainfo.Hash{2}, "other.mkv")] = treePayload{data: []byte("other"), created: created}
	tree := New(b)
	entries, err := tree.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if got := treeEntryNames(entries); !reflect.DeepEqual(got, []string{"Film.mkv", "Film.mkv-02000000", "empty", "tv"}) {
		t.Fatalf("root names = %v", got)
	}
	for _, entry := range entries[:2] {
		if entry.IsDir || entry.Identity == 0 {
			t.Fatalf("single file entry = %+v", entry)
		}
		handle, err := tree.Open(entry.Path)
		if err != nil {
			t.Fatal(err)
		}
		want := "film"
		if entry.Name != "Film.mkv" {
			want = "other"
		}
		if got := readTreeHandle(t, handle); got != want {
			t.Fatalf("%s = %q want %q", entry.Path, got, want)
		}
		_ = handle.Close()
	}
	if empty, err := tree.ReadDir("/empty"); err != nil || len(empty) != 0 {
		t.Fatalf("empty category = %v, %v", empty, err)
	}
	if entry, err := tree.Lookup("/tv/Series/season/E01.mkv"); err != nil || entry.IsDir || entry.Size != 6 || !entry.CreatedAt.Equal(created) {
		t.Fatalf("nested entry = %+v, %v", entry, err)
	}
}

func TestTreeOpenedMetadataAndSubtitleBytesStayTogether(t *testing.T) {
	hash := metainfo.Hash{4}
	oldTime, newTime := time.Unix(100, 0).UTC(), time.Unix(200, 0).UTC()
	b := newTreeFixture(TorrentView{Name: "Library", Hash: hash, FilesID: &FileListID{},
		Files:     []FileView{{Path: "video.mkv", Size: 0}},
		Subtitles: []SubtitleView{{Path: "video.srt", Size: 1, ModifiedAt: oldTime}}})
	b.payloads[treeFixtureKey(hash, "video.mkv")] = treePayload{data: []byte("generated payload"), created: newTime}
	b.subtitles[treeFixtureKey(hash, "video.srt")] = treeSubtitle{data: []byte("old subtitle"), modified: oldTime}
	tree := New(b)
	payload, err := tree.Open("/Library/video.mkv")
	if err != nil {
		t.Fatal(err)
	}
	defer closeTreeHandle(t, payload)
	if payload.Metadata().Size != int64(len("generated payload")) || !payload.Metadata().CreatedAt.Equal(newTime) {
		t.Fatalf("payload snapshot inherited listing metadata: %+v", payload.Metadata())
	}
	if got := readTreeHandle(t, payload); got != "generated payload" {
		t.Fatalf("payload = %q", got)
	}
	old, err := tree.Open("/Library/video.srt")
	if err != nil {
		t.Fatal(err)
	}
	defer closeTreeHandle(t, old)
	b.mu.Lock()
	b.subtitles[treeFixtureKey(hash, "video.srt")] = treeSubtitle{data: []byte("a much longer new subtitle"), modified: newTime}
	b.mu.Unlock()
	fresh, err := tree.Open("/Library/video.srt")
	if err != nil {
		t.Fatal(err)
	}
	defer closeTreeHandle(t, fresh)
	if got := readTreeHandle(t, old); got != "old subtitle" || !old.Metadata().ModifiedAt.Equal(oldTime) {
		t.Fatalf("old open changed bytes/metadata: %q %+v", got, old.Metadata())
	}
	if got := readTreeHandle(t, fresh); got != "a much longer new subtitle" || !fresh.Metadata().ModifiedAt.Equal(newTime) {
		t.Fatalf("new open mismatched bytes/metadata: %q %+v", got, fresh.Metadata())
	}
	stat, err := tree.Stat("/Library/video.srt")
	if err != nil || stat.Size != fresh.Metadata().Size || !stat.ModifiedAt.Equal(newTime) {
		t.Fatalf("live stat = %+v, %v", stat, err)
	}
}

func TestTreeDirectorySnapshotsRefreshMembershipWithoutChangingOldScans(t *testing.T) {
	hash := metainfo.Hash{5}
	b := newTreeFixture(TorrentView{Name: "Library", Hash: hash, FilesID: &FileListID{},
		Files: []FileView{{Path: "video.mkv", Size: 10}}})
	tree := New(b)
	old, err := tree.OpenDir("/Library")
	if err != nil {
		t.Fatal(err)
	}
	before := old.Entries()
	before[0].Name = "caller mutation"
	if got := old.Entries()[0].Name; got != "video.mkv" {
		t.Fatalf("Entries exposed mutable state: %q", got)
	}
	b.mu.Lock()
	b.views[0].Subtitles = []SubtitleView{{Path: "video.srt", Size: 9, ModifiedAt: time.Unix(200, 0)}}
	b.mu.Unlock()
	if _, err := tree.Lookup("/Library/video.srt"); err != nil {
		t.Fatalf("new subtitle absent after earlier lookup: %v", err)
	}
	fresh, err := tree.OpenDir("/Library")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Version() == old.Version() || !reflect.DeepEqual(treeEntryNames(fresh.Entries()), []string{"video.mkv", "video.srt"}) {
		t.Fatalf("new scan = version %d names %v", fresh.Version(), treeEntryNames(fresh.Entries()))
	}
	if got := treeEntryNames(old.Entries()); !reflect.DeepEqual(got, []string{"video.mkv"}) {
		t.Fatalf("in-flight scan changed: %v", got)
	}
	b.mu.Lock()
	b.views[0].Subtitles = nil
	b.mu.Unlock()
	if _, err := tree.Lookup("/Library/video.srt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed subtitle lookup = %v", err)
	}
	if b.opens.Load() != 0 {
		t.Fatalf("directory operations opened %d payloads", b.opens.Load())
	}
}

func TestTreeDirectoryVersionFollowsPayloadIdentityAndVirtualParent(t *testing.T) {
	hash := metainfo.Hash{6}
	created := time.Unix(100, 0).UTC()
	b := newTreeFixture(TorrentView{Name: "Old", Hash: hash, Category: "first", FilesID: &FileListID{},
		CreatedAt: created, Files: []FileView{{Path: "nested/old.bin", Size: 3}}})
	b.categories = []string{"first"}
	tree := New(b)
	old, err := tree.OpenDir("/first/Old/nested")
	if err != nil {
		t.Fatal(err)
	}
	oldChild := old.Entries()[0]
	b.mu.Lock()
	b.views[0].Name, b.views[0].Category = "Renamed", "second"
	b.categories = []string{"second"}
	b.mu.Unlock()
	moved, err := tree.OpenDir("/second/Renamed/nested")
	if err != nil {
		t.Fatal(err)
	}
	if moved.Version() == old.Version() || moved.Entries()[0].Path != "/second/Renamed/nested/old.bin" || moved.Entries()[0].Identity != oldChild.Identity {
		t.Fatalf("moved scan = %+v version %d", moved.Entries(), moved.Version())
	}
	if old.Entries()[0].Path != "/first/Old/nested/old.bin" {
		t.Fatal("move modified an opened directory snapshot")
	}
	if _, err := tree.Lookup("/first/Old/nested/old.bin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old path remained visible: %v", err)
	}
	b.mu.Lock()
	b.views[0].Files = []FileView{{Path: "nested/new.bin", Size: 5}}
	b.views[0].FilesID = &FileListID{}
	b.mu.Unlock()
	updated, err := tree.OpenDir("/second/Renamed/nested")
	if err != nil || updated.Version() == moved.Version() || !reflect.DeepEqual(treeEntryNames(updated.Entries()), []string{"new.bin"}) {
		t.Fatalf("replacement scan = %+v, %v", updated, err)
	}
	if _, err := tree.Lookup("/second/Renamed/nested/old.bin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old payload remained visible: %v", err)
	}
	b.mu.Lock()
	b.views[0].CreatedAt = created.Add(time.Hour)
	b.mu.Unlock()
	retimed, err := tree.OpenDir("/second/Renamed/nested")
	if err != nil || retimed.Version() == updated.Version() || !retimed.Entries()[0].CreatedAt.Equal(created.Add(time.Hour)) {
		t.Fatalf("retimed scan = %+v, %v", retimed, err)
	}
}

type treeContextKey struct{}

type treeContextReader struct {
	started chan context.Context
	closed  atomic.Int64
}

func (r *treeContextReader) ReadAt([]byte, int64) (int, error) {
	return 0, errors.New("context-aware reader fell back to ReadAt")
}
func (r *treeContextReader) ReadAtContext(ctx context.Context, _ []byte, _ int64) (int, error) {
	r.started <- ctx
	<-ctx.Done()
	return 0, ctx.Err()
}
func (r *treeContextReader) Close() error { r.closed.Add(1); return nil }

func TestTreeHandleForwardsCancellationAndCloseReclaimsOneReader(t *testing.T) {
	for _, closeHandle := range []bool{false, true} {
		t.Run(fmt.Sprintf("close=%v", closeHandle), func(t *testing.T) {
			hash := metainfo.Hash{7}
			b := newTreeFixture(TorrentView{Name: "Library", Hash: hash, FilesID: &FileListID{}, Files: []FileView{{Path: "blocked.bin", Size: 4}}})
			reader := &treeContextReader{started: make(chan context.Context, 1)}
			b.payloads[treeFixtureKey(hash, "blocked.bin")] = treePayload{data: []byte("data")}
			b.readers[treeFixtureKey(hash, "blocked.bin")] = reader
			handle, err := New(b).Open("/Library/blocked.bin")
			if err != nil {
				t.Fatal(err)
			}
			defer closeTreeHandle(t, handle)
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), treeContextKey{}, "request marker"))
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := handle.ReadAtContext(ctx, make([]byte, 4), 0); done <- err }()
			select {
			case observed := <-reader.started:
				if observed.Value(treeContextKey{}) != "request marker" {
					t.Fatal("request context values were lost")
				}
			case <-time.After(time.Second):
				t.Fatal("read did not enter the context-aware reader")
			}
			if closeHandle {
				closed := make(chan error, 1)
				go func() { closed <- handle.Close() }()
				select {
				case err := <-closed:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("Close did not interrupt active ReadAtContext")
				}
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("read cancellation = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("read did not honor cancellation")
			}
			if err := handle.Close(); err != nil {
				t.Fatal(err)
			}
			if err := handle.Close(); err != nil || reader.closed.Load() != 1 {
				t.Fatalf("Close = %v count %d", err, reader.closed.Load())
			}
			if _, err := handle.ReadAt(make([]byte, 1), 0); !errors.Is(err, fs.ErrClosed) {
				t.Fatalf("read after Close = %v", err)
			}
		})
	}
}

func TestTreeReadEOFAndRejectsInvalidVirtualOperations(t *testing.T) {
	hash := metainfo.Hash{8}
	b := newTreeFixture(TorrentView{Name: "Library", Hash: hash, FilesID: &FileListID{}, Files: []FileView{{Path: "file.bin", Size: 6}}})
	b.payloads[treeFixtureKey(hash, "file.bin")] = treePayload{data: []byte("abcdef")}
	tree := New(b)
	handle, err := tree.Open("/Library/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer closeTreeHandle(t, handle)
	buf := make([]byte, 8)
	if n, err := handle.ReadAt(buf, 3); n != 3 || !errors.Is(err, io.EOF) || string(buf[:n]) != "def" {
		t.Fatalf("short final read n=%d bytes=%q err=%v", n, buf, err)
	}
	if n, err := handle.ReadAt(buf, 6); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("EOF read n=%d err=%v", n, err)
	}
	if _, err := handle.ReadAt(buf, -1); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("negative offset = %v", err)
	}
	for _, path := range []string{"/../Library", "/Library/../file.bin", "/Library/./file.bin", "/Library/file\x00.bin"} {
		if _, err := tree.Lookup(path); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("invalid virtual path %q = %v", path, err)
		}
	}
	if _, err := tree.Open("/Library"); !errors.Is(err, ErrIsDir) {
		t.Fatalf("directory Open = %v", err)
	}
	if _, err := tree.ReadDir("/Library/file.bin"); !errors.Is(err, ErrNotDir) {
		t.Fatalf("file ReadDir = %v", err)
	}
	if _, err := tree.Lookup("/Library/file.bin/child"); !errors.Is(err, ErrNotDir) {
		t.Fatalf("file descendant = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := handle.ReadAtContext(ctx, buf, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled read = %v", err)
	}
}

func TestTreeHotLookupKeepsImmutableTwentyThousandFileIndex(t *testing.T) {
	files := make([]FileView, 20000)
	for i := range files {
		files[i] = FileView{Path: fmt.Sprintf("file-%05d.bin", i), Size: int64(i)}
	}
	b := newTreeFixture(TorrentView{Name: "Library", Hash: metainfo.Hash{9}, FilesID: &FileListID{}, Files: files})
	tree := New(b)
	first, err := tree.OpenDir("/Library")
	if err != nil || len(first.Entries()) != len(files) {
		t.Fatalf("initial directory = %+v, %v", first, err)
	}
	version := first.Version()
	allocs := testing.AllocsPerRun(25, func() {
		entry, err := tree.Lookup("/Library/file-19999.bin")
		if err != nil || entry.Size != 19999 {
			t.Fatalf("hot lookup = %+v, %v", entry, err)
		}
		current, err := tree.OpenDir("/Library")
		if err != nil || current != first || current.Version() != version {
			t.Fatalf("hot directory rebuilt immutable snapshot: %p/%p, %v", current, first, err)
		}
	})
	// A per-lookup 20,000-entry rebuild allocates tens of thousands of objects.
	// Leave headroom for root/category naming while rejecting that regression.
	if allocs > 200 {
		t.Fatalf("hot lookup allocations = %.0f, want <= 200", allocs)
	}
	if b.opens.Load() != 0 {
		t.Fatal("metadata lookup opened payload bytes")
	}
	if !strings.HasSuffix(first.Entries()[19999].Path, "file-19999.bin") {
		t.Fatal("large-directory snapshot lost its last entry")
	}
}
