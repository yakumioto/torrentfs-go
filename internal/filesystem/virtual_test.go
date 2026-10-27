package filesystem

import (
	"errors"
	"io"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"time"
)

func virtualNames(entries []VirtualEntry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	return names
}

func TestVirtualNamespaceLayoutAndRefresh(t *testing.T) {
	b := &fakeBackend{categories: []string{"Films", "Empty"}}
	v := NewVirtualFS(b)
	entries, err := v.ReadDir("")
	if err != nil || !reflect.DeepEqual(virtualNames(entries), []string{"Empty", "Films"}) {
		t.Fatalf("empty library: %v %v", entries, err)
	}
	a := TorrentView{Name: "same.mp4", Hash: hashN(1), SingleFile: true}
	b.addFile(&a, "same.mp4", []byte("first"))
	b.addSubtitle(&a, "same.srt", "same.mp4", []byte("subtitle"))
	other := TorrentView{Name: "same.mp4", Hash: hashN(2), SingleFile: true}
	b.addFile(&other, "same.mp4", []byte("second"))
	multi := TorrentView{Name: "Collection", Hash: hashN(3), Category: "Films"}
	b.addFile(&multi, "嵌套😀/电影.mkv", []byte("movie"))
	b.views = []TorrentView{other, multi, a}
	entries, err = v.ReadDir("")
	want := []string{"Empty", "Films", "same.mp4", "same.mp4-02000000", "same.srt"}
	if err != nil || !reflect.DeepEqual(virtualNames(entries), want) {
		t.Fatalf("root: %v %v", virtualNames(entries), err)
	}
	if entries[2].IsDir || entries[2].Size != 5 {
		t.Fatalf("single file: %+v", entries[2])
	}
	r, stat, err := v.Open("Films/Collection/嵌套😀/电影.mkv")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(io.NewSectionReader(r, 0, stat.Size))
	if err != nil || string(data) != "movie" {
		t.Fatalf("nested read: %q %v", data, err)
	}
	entries, err = v.ReadDir("Empty")
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty category: %v %v", entries, err)
	}
	for _, bad := range []string{"../x", "Films/../x", "/etc/passwd", "Films//x", "x\\y", "bad\x00name"} {
		if _, err := v.Stat(bad); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("unsafe %q: %v", bad, err)
		}
	}
	for _, hidden := range []string{".metadata", "registry.json", "Films/Collection/missing"} {
		if _, err := v.Stat(hidden); !errors.Is(err, ErrNotFound) {
			t.Errorf("hidden/missing %q: %v", hidden, err)
		}
	}
	a.Category = "Films"
	b.views = []TorrentView{a}
	if _, err := v.Stat("same.mp4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old category path: %v", err)
	}
	if _, err := v.Stat("Films/same.mp4"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Stat("Films/Collection/嵌套😀"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted torrent: %v", err)
	}
}

func TestVirtualOpenBindsSubtitleAndGeneratedSnapshot(t *testing.T) {
	b := &fakeBackend{}
	view := TorrentView{Name: "movie.mp4", Hash: hashN(1), SingleFile: true}
	b.addFile(&view, "movie.mp4", []byte("video"))
	b.addSubtitle(&view, "movie.srt", "movie.mp4", []byte("old"))
	b.views = []TorrentView{view}
	v := NewVirtualFS(b)
	old, before, err := v.Open("movie.srt")
	if err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(100, 0)
	b.replaceSubtitle(view.Hash, "movie.srt", []byte("new subtitle"), mtime)
	fresh, after, err := v.Open("movie.srt")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		r    io.ReaderAt
		size int64
		want string
	}{{old, before.Size, "old"}, {fresh, after.Size, "new subtitle"}} {
		got, err := io.ReadAll(io.NewSectionReader(tc.r, 0, tc.size))
		if err != nil || string(got) != tc.want {
			t.Fatalf("snapshot: %q %v", got, err)
		}
	}
	if !after.ModifiedAt.Equal(mtime) || after.Size != 12 {
		t.Fatalf("new metadata: %+v", after)
	}
	generated := &fileSnapshotTestBackend{fakeBackend: b, statErr: ErrNotFound,
		snapshot: FileSnapshot{Reader: strings.NewReader("signed link\n"), Size: 12}}
	view.Files[0].Size = 0
	generated.views = []TorrentView{view}
	_, stat, err := NewVirtualFS(generated).Open("movie.mp4")
	if err != nil || stat.Size != 12 || generated.statCalls != 0 || generated.payloadCalls != 0 {
		t.Fatalf("generated snapshot: %+v %v", stat, err)
	}
	generated.snapshotErr = ErrPermission
	if _, _, err := NewVirtualFS(generated).Open("movie.mp4"); !errors.Is(err, ErrPermission) {
		t.Fatalf("unavailable generated file: %v", err)
	}
}
