package vfs

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

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
