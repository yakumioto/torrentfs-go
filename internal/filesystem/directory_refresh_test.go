package filesystem

import (
	"encoding/binary"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// rawDirectory keeps the bridge's actual directory handle and offset, rather
// than calling a newly constructed node that the bridge might discard.
type rawDirectory struct {
	raw    fuse.RawFileSystem
	node   uint64
	handle uint64
	offset uint64
}

func openRawDirectory(t *testing.T, raw fuse.RawFileSystem, node uint64) *rawDirectory {
	t.Helper()
	var out fuse.OpenOut
	if status := raw.OpenDir(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: node}}, &out); status != fuse.OK {
		t.Fatalf("Raw.OpenDir node %d = %v", node, status)
	}
	dir := &rawDirectory{raw: raw, node: node, handle: out.Fh}
	t.Cleanup(func() { raw.ReleaseDir(&fuse.ReleaseIn{InHeader: fuse.InHeader{NodeId: node}, Fh: out.Fh}) })
	return dir
}

func (d *rawDirectory) read(t *testing.T, size int) []string {
	t.Helper()
	buf := make([]byte, size)
	list := fuse.NewDirEntryList(buf, d.offset)
	if status := d.raw.ReadDir(nil, &fuse.ReadIn{
		InHeader: fuse.InHeader{NodeId: d.node}, Fh: d.handle, Offset: d.offset, Size: uint32(size),
	}, list); status != fuse.OK {
		t.Fatalf("Raw.ReadDir node %d offset %d = %v", d.node, d.offset, status)
	}
	d.offset = list.Offset
	var names []string
	// FUSE dirents have a 24-byte header and an eight-byte-aligned name.
	for len(buf) >= 24 {
		n := int(binary.NativeEndian.Uint32(buf[16:20]))
		if n == 0 {
			break
		}
		length := 24 + (n+7)&^7
		if length > len(buf) {
			t.Fatal("invalid serialized FUSE dirent")
		}
		names = append(names, string(buf[24:24+n]))
		buf = buf[length:]
	}
	return names
}

func TestTorrentDirectoryOldBridgeInodesSeeSubtitleUpdates(t *testing.T) {
	for _, strm := range []bool{false, true} {
		name := "original"
		if strm {
			name = "strm"
		}
		t.Run(name, func(t *testing.T) {
			fake := &fakeBackend{views: []TorrentView{{Name: "Collection", Hash: hashN(1)}}}
			for _, path := range []string{"top.mkv", "season/deep/Movie.mkv", "season/deep/z-last.txt"} {
				fake.addFile(&fake.views[0], path, []byte("payload"))
			}
			var backend Backend = fake
			if strm {
				backend = NewSTRMBackend(&strmTestBackend{fakeBackend: fake})
			}
			root := &rootNode{state: newFSState(backend)}
			raw := fs.NewNodeFS(root, nil)
			top := fileSnapshotRawLookup(t, raw, 1, "Collection", fuse.OK).NodeId
			season := fileSnapshotRawLookup(t, raw, top, "season", fuse.OK).NodeId
			deep := fileSnapshotRawLookup(t, raw, season, "deep", fuse.OK).NodeId
			oldTop := openRawDirectory(t, raw, top)
			if got := oldTop.read(t, 4096); len(got) != 2 {
				t.Fatalf("initial top listing = %v, want two entries", got)
			}
			oldDeep := openRawDirectory(t, raw, deep)
			first := oldDeep.read(t, 40)
			if len(first) != 1 {
				t.Fatalf("first directory page = %v, want one entry", first)
			}

			fake.addSubtitle(&fake.views[0], "top.srt", "top.mkv", []byte("top subtitle"))
			fake.addSubtitle(&fake.views[0], "season/deep/Movie.srt", "season/deep/Movie.mkv", []byte("old subtitle"))
			// addNewChild must actually reuse each previously visited parent inode.
			if again := fileSnapshotRawLookup(t, raw, 1, "Collection", fuse.OK).NodeId; again != top {
				t.Fatalf("top NodeId = %d, want retained %d", again, top)
			}
			if again := fileSnapshotRawLookup(t, raw, top, "season", fuse.OK).NodeId; again != season {
				t.Fatalf("season NodeId = %d, want retained %d", again, season)
			}
			if again := fileSnapshotRawLookup(t, raw, season, "deep", fuse.OK).NodeId; again != deep {
				t.Fatalf("deep NodeId = %d, want retained %d", again, deep)
			}
			fileSnapshotRawLookup(t, raw, top, "top.srt", fuse.OK)
			subtitle := fileSnapshotRawLookup(t, raw, deep, "Movie.srt", fuse.OK)
			fileSnapshotRawRead(t, raw, subtitle.NodeId, "old subtitle")
			want := []string{"Movie.mkv", "Movie.srt", "z-last.txt"}
			if strm {
				want[0] = "Movie.strm"
				slices.Sort(want)
			}
			if got := openRawDirectory(t, raw, deep).read(t, 4096); !reflect.DeepEqual(got, want) {
				t.Fatalf("new listing on old inode = %v, want %v", got, want)
			}
			if got := oldTop.read(t, 4096); len(got) != 0 {
				t.Fatalf("completed old stream gained entries: %v", got)
			}
			if got := oldDeep.read(t, 4096); !reflect.DeepEqual(got, []string{"z-last.txt"}) {
				t.Fatalf("old stream's next page = %v, want unchanged offset and last entry", got)
			}
			for _, text := range []string{"new subtitle", "a much longer replacement subtitle"} {
				fake.replaceSubtitle(hashN(1), "season/deep/Movie.srt", []byte(text), time.Now())
				fileSnapshotRawRead(t, raw, subtitle.NodeId, text)
			}
		})
	}
}
