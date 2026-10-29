package smb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/vfs"
)

func TestSMBPortableNamesRemainDistinctAndStable(t *testing.T) {
	originals := []string{
		"Movie.mkv", "movie.MKV", "Movie.mkv~0000000000000001",
		"s.txt", "ſ.txt", "Σ.txt", "ς.txt",
		"bad:name?.txt", "bad%3Aname%3F.txt", "back\\slash.bin",
		"CON", "con.txt", "PRN", "aux.log", "NUL", "COM1.bin", "LPT9",
		"trailing.", "trailing ", "line\nfeed.txt", "literal[1].txt",
	}
	entries := make([]vfs.Entry, len(originals))
	for i, name := range originals {
		entries[i] = vfs.Entry{Name: name, Path: "/original/" + name, Identity: uint64(i + 1)}
	}
	mapped := smbNames(entries)
	seen := make(map[string]bool)
	byIdentity := make(map[uint64]vfs.Entry)
	for _, entry := range mapped {
		if seen[foldName(entry.Name)] {
			t.Fatalf("case-insensitive collision: %q", entry.Name)
		}
		seen[foldName(entry.Name)] = true
		byIdentity[entry.Identity] = entry
		if _, err := pathParts(entry.Name); err != nil {
			t.Fatalf("generated illegal SMB name %q: %v", entry.Name, err)
		}
		if utf16Length(entry.Name) > 255 || strings.HasSuffix(entry.Name, ".") || strings.HasSuffix(entry.Name, " ") {
			t.Fatalf("generated unportable name %q", entry.Name)
		}
		if entry.Path != entries[entry.Identity-1].Path {
			t.Fatalf("alias changed original virtual identity: %+v", entry)
		}
	}
	if len(mapped) != len(entries) {
		t.Fatal("mapping lost an entry")
	}
	if foldName("s") != foldName("ſ") || foldName("Σ") != foldName("ς") {
		t.Fatal("SMB folding does not include Unicode simple-fold cycles")
	}
	if byIdentity[8].Name == byIdentity[9].Name || !strings.Contains(byIdentity[9].Name, "%25") {
		t.Fatalf("literal percent could impersonate an escaped name: %q %q", byIdentity[8].Name, byIdentity[9].Name)
	}
	for i, original := range originals {
		if entries[i].Name != original {
			t.Fatalf("mapping modified caller entry %d", i)
		}
	}
	slices.Reverse(entries)
	for _, entry := range smbNames(entries) {
		if entry.Name != byIdentity[entry.Identity].Name {
			t.Fatalf("input ordering changed alias for identity %d: %q vs %q", entry.Identity, entry.Name, byIdentity[entry.Identity].Name)
		}
	}
}

func TestSMBCollisionAliasesPreserveMediaEndings(t *testing.T) {
	entries := []vfs.Entry{
		{Name: "Movie.mkv", Identity: 1}, {Name: "movie.MKV", Identity: 2},
		{Name: "Disc.iso.strm", Identity: 3}, {Name: "disc.ISO.STRM", Identity: 4},
		{Name: strings.Repeat("中", 300) + ".mp4", Identity: 5},
		{Name: strings.Repeat("😀", 200) + ".iso.strm", Identity: 6},
	}
	for _, entry := range smbNames(entries) {
		name := strings.ToLower(entry.Name)
		want := ".mkv"
		switch entry.Identity {
		case 3, 4, 6:
			want = ".iso.strm"
		case 5:
			want = ".mp4"
		}
		if !strings.HasSuffix(name, want) {
			t.Errorf("media alias %q lost %s ending", entry.Name, want)
		}
		if utf16Length(entry.Name) > 255 || !utf8.ValidString(entry.Name) {
			t.Errorf("media alias exceeds UTF-16 limit or splits UTF-8: %q", entry.Name)
		}
	}
}

func TestSMBPathRejectsTraversalDriveAndADS(t *testing.T) {
	for _, raw := range []string{
		`..\video.mkv`, `Library\..\video.mkv`, `Library/../../video.mkv`,
		`C:\video.mkv`, `video.mkv:other`, `video.mkv::$DATA`, `\\server\share\video.mkv`,
		"//server/share/video.mkv", "video\x00.mkv", `bad<name`, `bad>name`, `bad"name`, `bad|name`, `bad?name`, `bad*name`,
	} {
		if _, err := pathParts(raw); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("unsafe wire path %q = %v", raw, err)
		}
	}
	for raw, want := range map[string][]string{
		"": nil, `\`: nil, ".": nil,
		`Library\season\E01.mkv`:     {"Library", "season", "E01.mkv"},
		`/Library//season/./E01.mkv`: {"Library", "season", "E01.mkv"},
	} {
		got, err := pathParts(raw)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("wire path %q = %v/%v want %v", raw, got, err, want)
		}
	}
}

func TestSMBWildcardTreatsBracketsAsLiterals(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"*.*", "README", true}, {"*.MKV", "movie.mkv", true},
		{"video[1].*", "video[1].mkv", true}, {"video[1].*", "video1.mkv", false},
		{"?.srt", "中.srt", true}, {"a?c", "ac", false},
		{"s.TXT", "ſ.txt", true}, {"*.strm", "video.mkv", false},
	} {
		if got := wildcardMatch(tc.pattern, tc.name); got != tc.want {
			t.Errorf("wildcard %q / %q = %v want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

type namesFixture struct {
	view  vfs.TorrentView
	opens int
}

func (b *namesFixture) Torrents() []vfs.TorrentView { return []vfs.TorrentView{b.view} }
func (b *namesFixture) TorrentContents(hash metainfo.Hash) (vfs.TorrentContents, error) {
	if hash != b.view.Hash {
		return vfs.TorrentContents{}, vfs.ErrNotFound
	}
	return vfs.TorrentContents{Files: b.view.Files, FilesID: b.view.FilesID,
		Subtitles: slices.Clone(b.view.Subtitles), CreatedAt: b.view.CreatedAt, SingleFile: b.view.SingleFile}, nil
}
func (b *namesFixture) OpenFile(_ metainfo.Hash, name string) (io.ReaderAt, error) {
	for _, file := range b.view.Files {
		if file.Path == name {
			b.opens++
			return bytes.NewReader([]byte(name)), nil
		}
	}
	return nil, vfs.ErrNotFound
}

func TestSMBAliasesResolveToTheirOwnVirtualFile(t *testing.T) {
	b := &namesFixture{view: vfs.TorrentView{Name: "Library", Hash: metainfo.Hash{41}, FilesID: &vfs.FileListID{}}}
	for _, path := range []string{"A.txt", "a.TXT", "s.txt", "ſ.txt", "bad:name.txt", "bad%3Aname.txt", "back\\slash.bin", "CON", "trail."} {
		b.view.Files = append(b.view.Files, vfs.FileView{Path: path, Size: int64(len(path))})
	}
	tree := vfs.New(b)
	adapter := &filesystem{tree: tree}
	directory, err := tree.OpenDir("/Library")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range adapter.nameIndex(directory).entries {
		entry, _, err := adapter.resolve(`LIBRARY\` + expected.Name)
		if err != nil || entry.Path != expected.Path || entry.Identity != expected.Identity {
			t.Fatalf("alias %q resolved to %+v/%v instead of %+v", expected.Name, entry, err, expected)
		}
		handle, err := tree.Open(entry.Path)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, len(entry.Path)+8)
		n, readErr := handle.ReadAt(buf, 0)
		_ = handle.Close()
		if !errors.Is(readErr, io.EOF) || string(buf[:n]) != strings.TrimPrefix(expected.Path, "/Library/") {
			t.Fatalf("alias %q read %q/%v", expected.Name, buf[:n], readErr)
		}
	}
}

func TestSMBNameIndexRefreshesAndDoesNotRepublishOlderSnapshot(t *testing.T) {
	b := &namesFixture{view: vfs.TorrentView{Name: "Library", Hash: metainfo.Hash{42}, FilesID: &vfs.FileListID{}, Files: []vfs.FileView{{Path: "old.bin", Size: 7}}}}
	tree := vfs.New(b)
	adapter := &filesystem{tree: tree}
	old, err := tree.OpenDir("/Library")
	if err != nil {
		t.Fatal(err)
	}
	oldIndex := adapter.nameIndex(old)
	b.view.Files, b.view.FilesID = []vfs.FileView{{Path: "new.bin", Size: 7}}, &vfs.FileListID{}
	fresh, err := tree.OpenDir("/Library")
	if err != nil {
		t.Fatal(err)
	}
	freshIndex := adapter.nameIndex(fresh)
	if freshIndex == oldIndex || freshIndex.byName[foldName("new.bin")].Path != "/Library/new.bin" {
		t.Fatal("new directory version reused stale wire names")
	}
	if oldIndex.byName[foldName("old.bin")].Path != "/Library/old.bin" {
		t.Fatal("new directory changed old immutable wire names")
	}
	adapter.nameIndex(old)
	if got := adapter.nameIndex(fresh); got != freshIndex {
		t.Fatal("an old scan displaced the newer name index")
	}
	if _, _, err := adapter.resolve(`Library\OLD.BIN`); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deleted name remained accessible: %v", err)
	}
}

func TestSMBHotResolveKeepsTwentyThousandFileNameIndex(t *testing.T) {
	b := &namesFixture{view: vfs.TorrentView{Name: "Library", Hash: metainfo.Hash{43}, FilesID: &vfs.FileListID{}}}
	for i := 0; i < 20000; i++ {
		name := fmt.Sprintf("file-%05d.bin", i)
		b.view.Files = append(b.view.Files, vfs.FileView{Path: name, Size: int64(i)})
	}
	tree := vfs.New(b)
	adapter := &filesystem{tree: tree}
	if _, _, err := adapter.resolve(`Library\FILE-19999.BIN`); err != nil {
		t.Fatal(err)
	}
	directory, err := tree.OpenDir("/Library")
	if err != nil {
		t.Fatal(err)
	}
	index := adapter.nameIndex(directory)
	allocs := testing.AllocsPerRun(20, func() {
		entry, _, err := adapter.resolve(`LIBRARY\FILE-19999.BIN`)
		if err != nil || entry.Size != 19999 {
			t.Fatalf("hot resolve = %+v/%v", entry, err)
		}
	})
	if allocs > 1000 {
		t.Fatalf("hot resolve allocated %.0f objects; immutable directory names were rebuilt", allocs)
	}
	if adapter.nameIndex(directory) != index || b.opens != 0 {
		t.Fatal("metadata resolution rebuilt a stable index or opened payload bytes")
	}
}
