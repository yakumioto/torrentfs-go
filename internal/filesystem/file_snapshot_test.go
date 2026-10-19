package filesystem

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type fileSnapshotTestBackend struct {
	*fakeBackend
	stat          FileStat
	statErr       error
	snapshot      FileSnapshot
	snapshotErr   error
	statCalls     int
	snapshotCalls int
	payloadCalls  int
}

func (b *fileSnapshotTestBackend) FileStat(metainfo.Hash, string) (FileStat, error) {
	b.statCalls++
	return b.stat, b.statErr
}

func (b *fileSnapshotTestBackend) OpenFileSnapshot(metainfo.Hash, string) (FileSnapshot, error) {
	b.snapshotCalls++
	return b.snapshot, b.snapshotErr
}

func (b *fileSnapshotTestBackend) OpenFile(hash metainfo.Hash, path string) (io.ReaderAt, error) {
	b.payloadCalls++
	return b.fakeBackend.OpenFile(hash, path)
}

func TestPayloadSnapshotOpenUsesItsOwnSizeWithoutASeparateStat(t *testing.T) {
	ctx := context.Background()
	createdAt := time.Date(2026, 10, 20, 1, 2, 3, 0, time.UTC)
	text := "http://media.test/" + strings.Repeat("signed-content-", 40) + "\n"
	backend := &fileSnapshotTestBackend{
		fakeBackend: &fakeBackend{},
		statErr:     ErrNotFound,
		snapshot:    FileSnapshot{Reader: strings.NewReader(text), Size: int64(len(text)), CreatedAt: createdAt},
	}
	// An old inode may have copied zero from an unavailable directory snapshot.
	node := &torrentFileNode{state: newFSState(backend), hash: hashN(1), path: "Movie.strm", size: 0}
	handle, flags, errno := node.Open(ctx, syscall.O_RDONLY)
	if errno != 0 || flags != 0 {
		t.Fatalf("snapshot Open flags=%d errno=%v", flags, errno)
	}
	read := handle.(*readHandle)
	if read.size != int64(len(text)) {
		t.Fatalf("open read bound = %d, want snapshot length %d", read.size, len(text))
	}
	buf := make([]byte, len(text)+16)
	result, errno := read.Read(ctx, buf, 0)
	if errno != 0 {
		t.Fatalf("snapshot Read errno = %v", errno)
	}
	got, status := result.Bytes(buf)
	if status != fuse.OK || string(got) != text {
		t.Fatalf("snapshot read = %d bytes/%v, want all %d bytes", len(got), status, len(text))
	}
	if backend.statCalls != 0 || backend.snapshotCalls != 1 || backend.payloadCalls != 0 {
		t.Fatalf("Open called stat/snapshot/payload = %d/%d/%d, want 0/1/0", backend.statCalls, backend.snapshotCalls, backend.payloadCalls)
	}
	backend.statErr = nil
	backend.stat = FileStat{Size: int64(len(text)), CreatedAt: createdAt}
	var attr fuse.AttrOut
	if errno := node.Getattr(ctx, nil, &attr); errno != 0 || attr.Size != uint64(len(text)) {
		t.Fatalf("recovered Getattr size=%d errno=%v", attr.Size, errno)
	}
	assertAttrTimes(t, &attr.Attr, createdAt)
	backend.statErr = ErrPermission
	if errno := node.Getattr(ctx, nil, &fuse.AttrOut{}); errno != syscall.EACCES {
		t.Fatalf("failed Getattr errno = %v, want EACCES", errno)
	}
}

type fileSnapshotTestCloser struct {
	*strings.Reader
	closed bool
}

func (r *fileSnapshotTestCloser) Close() error {
	r.closed = true
	return nil
}

func TestPayloadSnapshotRejectsInvalidReadersSizesAndErrors(t *testing.T) {
	for _, name := range []string{"nil-reader", "negative-size", "backend-error"} {
		t.Run(name, func(t *testing.T) {
			reader := &fileSnapshotTestCloser{Reader: strings.NewReader("content")}
			backend := &fileSnapshotTestBackend{
				fakeBackend: &fakeBackend{},
				snapshot:    FileSnapshot{Reader: reader, Size: 7},
			}
			want := syscall.EINVAL
			switch name {
			case "nil-reader":
				backend.snapshot.Reader = nil
			case "negative-size":
				backend.snapshot.Size = -1
			case "backend-error":
				backend.snapshot = FileSnapshot{}
				backend.snapshotErr = ErrPermission
				want = syscall.EACCES
			}
			node := &torrentFileNode{state: newFSState(backend), hash: hashN(1), path: "Movie.strm"}
			if handle, _, errno := node.Open(context.Background(), syscall.O_RDONLY); handle != nil || errno != want {
				t.Fatalf("invalid snapshot Open = %T/%v, want nil/%v", handle, errno, want)
			}
			if name == "negative-size" && !reader.closed {
				t.Fatal("invalid snapshot reader was not closed")
			}
			if backend.payloadCalls != 0 || backend.statCalls != 0 {
				t.Fatalf("invalid snapshot fell back to payload or stat: %d/%d", backend.payloadCalls, backend.statCalls)
			}
		})
	}
}

func TestPayloadMetadataRejectsNegativeSizesInEveryLookupLayout(t *testing.T) {
	for _, layout := range []string{"root", "category", "nested"} {
		for _, live := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/live-%v", layout, live), func(t *testing.T) {
				view := TorrentView{Name: "Movie.mkv", Hash: hashN(1), SingleFile: layout != "nested", Files: []FileView{{Path: "Movie.mkv", Size: -1}}}
				fake := &fakeBackend{views: []TorrentView{view}}
				if layout == "category" {
					fake.categories = []string{"movies"}
					fake.views[0].Category = "movies"
				}
				if layout == "nested" {
					fake.views[0].Files[0].Path = "nested/Movie.mkv"
				}
				var backend Backend = fake
				var provider *fileSnapshotTestBackend
				if live {
					fake.views[0].Files[0].Size = 7
					provider = &fileSnapshotTestBackend{fakeBackend: fake, stat: FileStat{Size: -1}}
					backend = provider
				}
				root := &rootNode{state: newFSState(backend)}
				raw := fs.NewNodeFS(root, nil)
				parent := uint64(1)
				if layout == "category" {
					parent = fileSnapshotRawLookup(t, raw, parent, "movies", fuse.OK).NodeId
				}
				if layout == "nested" {
					parent = fileSnapshotRawLookup(t, raw, parent, "Movie.mkv", fuse.OK).NodeId
					parent = fileSnapshotRawLookup(t, raw, parent, "nested", fuse.OK).NodeId
				}
				if provider != nil && provider.statCalls != 0 {
					t.Fatal("directory lookup called payload FileStat")
				}
				fileSnapshotRawLookup(t, raw, parent, "Movie.mkv", fuse.Status(syscall.EINVAL))
				if provider != nil && provider.statCalls != 1 {
					t.Fatalf("payload stat calls = %d, want 1", provider.statCalls)
				}
			})
		}
	}
	backend := &fakeBackend{}
	node := &torrentFileNode{state: newFSState(backend), hash: hashN(1), path: "negative", size: -1}
	if _, _, errno := node.Open(context.Background(), syscall.O_RDONLY); errno != syscall.EINVAL {
		t.Fatalf("negative fallback Open errno = %v, want EINVAL", errno)
	}
	if errno := node.Getattr(context.Background(), nil, &fuse.AttrOut{}); errno != syscall.EINVAL {
		t.Fatalf("negative fallback Getattr errno = %v, want EINVAL", errno)
	}
}

func TestPayloadCapabilitiesNeverInterceptManagedSubtitles(t *testing.T) {
	ctx := context.Background()
	backend := &fileSnapshotTestBackend{
		fakeBackend: &fakeBackend{views: []TorrentView{{Name: "Movie.mkv", Hash: hashN(1), SingleFile: true}}},
		statErr:     ErrPermission,
		snapshotErr: ErrPermission,
	}
	backend.addFile(&backend.views[0], "Movie.mkv", []byte("video"))
	backend.addSubtitle(&backend.views[0], "Movie.srt", "Movie.mkv", []byte("old"))
	root := &rootNode{state: newFSState(backend)}
	_ = fs.NewNodeFS(root, nil)
	inode, errno := root.Lookup(ctx, "Movie.srt", &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("subtitle Lookup errno = %v", errno)
	}
	node := inode.Operations().(*torrentFileNode)
	old, _, errno := node.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("old subtitle Open errno = %v", errno)
	}
	modified := time.Date(2026, 10, 20, 2, 3, 4, 0, time.UTC)
	backend.replaceSubtitle(hashN(1), "Movie.srt", []byte("longer replacement"), modified)
	var attr fuse.AttrOut
	if errno := node.Getattr(ctx, nil, &attr); errno != 0 || attr.Size != uint64(len("longer replacement")) {
		t.Fatalf("subtitle live Getattr size=%d errno=%v", attr.Size, errno)
	}
	handle, flags, errno := node.Open(ctx, syscall.O_RDONLY)
	if errno != 0 || flags != fuse.FOPEN_DIRECT_IO {
		t.Fatalf("subtitle Open flags=%d errno=%v", flags, errno)
	}
	if got := strmTestRead(t, handle.(*readHandle).ra); got != "longer replacement" {
		t.Fatalf("new subtitle content = %q", got)
	}
	if got := strmTestRead(t, old.(*readHandle).ra); got != "old" {
		t.Fatalf("old subtitle snapshot = %q", got)
	}
	if backend.statCalls != 0 || backend.snapshotCalls != 0 || backend.payloadCalls != 0 {
		t.Fatalf("subtitle used payload stat/snapshot/open: %d/%d/%d", backend.statCalls, backend.snapshotCalls, backend.payloadCalls)
	}
}

func fileSnapshotRawLookup(t *testing.T, raw fuse.RawFileSystem, parent uint64, name string, want fuse.Status) fuse.EntryOut {
	t.Helper()
	var out fuse.EntryOut
	if status := raw.Lookup(nil, &fuse.InHeader{NodeId: parent}, name, &out); status != want {
		t.Fatalf("Raw.Lookup %q status=%v, want %v", name, status, want)
	}
	if want != fuse.OK && out.NodeId != 0 {
		t.Fatalf("failed Raw.Lookup published node %d", out.NodeId)
	}
	return out
}

func fileSnapshotRawRead(t *testing.T, raw fuse.RawFileSystem, node uint64, want string) {
	t.Helper()
	header := fuse.InHeader{NodeId: node}
	var opened fuse.OpenOut
	if status := raw.Open(nil, &fuse.OpenIn{InHeader: header, Flags: syscall.O_RDONLY}, &opened); status != fuse.OK {
		t.Fatalf("Raw.Open status = %v", status)
	}
	defer raw.Release(nil, &fuse.ReleaseIn{InHeader: header, Fh: opened.Fh})
	for _, offset := range []int{0, len(want) - 8} {
		buf := make([]byte, len(want)+16)
		result, status := raw.Read(nil, &fuse.ReadIn{InHeader: header, Fh: opened.Fh, Offset: uint64(offset), Size: uint32(len(buf))}, buf)
		if status != fuse.OK {
			t.Fatalf("Raw.Read offset %d status = %v", offset, status)
		}
		got, status := result.Bytes(buf)
		result.Done()
		if status != fuse.OK || string(got) != want[offset:] {
			t.Fatalf("Raw.Read offset %d returned %d bytes/%v, want %d exact bytes", offset, len(got), status, len(want)-offset)
		}
	}
}

type fileSnapshotVaryingURLBackend struct {
	*strmTestBackend
	suffix string
}

func (b *fileSnapshotVaryingURLBackend) StreamURL(hash metainfo.Hash, path string) (string, error) {
	link, err := b.strmTestBackend.StreamURL(hash, path)
	if err != nil {
		return "", err
	}
	return link + b.suffix, nil
}

func TestSTRMRawBridgeRecoversUnavailableSnapshotsAndReusesOldInodes(t *testing.T) {
	for _, layout := range []string{"root-single", "category-single", "nested-multi"} {
		t.Run(layout, func(t *testing.T) {
			created := time.Date(2026, 10, 20, 3, 4, 5, 0, time.UTC)
			view := TorrentView{Name: "Movie.mkv", Hash: hashN(1), SingleFile: layout != "nested-multi", CreatedAt: created}
			original, leaf := "Movie.mkv", "Movie.strm"
			if layout == "nested-multi" {
				view.Name = "Collection"
				original = "nested/Movie.mkv"
			}
			if layout == "category-single" {
				view.Category = "movies"
			}
			backend := &fileSnapshotVaryingURLBackend{strmTestBackend: &strmTestBackend{fakeBackend: &fakeBackend{
				views:      []TorrentView{view, {Name: "Healthy.mkv", Hash: hashN(2), SingleFile: true}},
				categories: []string{"movies"},
			}}}
			backend.addFile(&backend.views[0], original, []byte("do not open video"))
			backend.addFile(&backend.views[1], "Healthy.mkv", []byte("do not open healthy video"))
			errorKey := hashN(1).HexString() + "\x00" + original
			backend.streamErrors = map[string]error{errorKey: ErrPermission}
			projection := NewSTRMBackend(backend)
			if file := projection.Torrents()[0].Files[0]; file.Size != 0 {
				t.Fatalf("failed listing size = %d, want zero", file.Size)
			}
			root := &rootNode{state: newFSState(projection)}
			raw := fs.NewNodeFS(root, nil)
			parent := uint64(1)
			switch layout {
			case "category-single":
				parent = fileSnapshotRawLookup(t, raw, parent, "movies", fuse.OK).NodeId
			case "nested-multi":
				parent = fileSnapshotRawLookup(t, raw, parent, "Collection", fuse.OK).NodeId
				parent = fileSnapshotRawLookup(t, raw, parent, "nested", fuse.OK).NodeId
			}
			// Keep these bridge parent nodes alive: the multi-file directory still
			// contains the zero-sized listing even after the backend recovers.
			fileSnapshotRawLookup(t, raw, parent, leaf, fuse.Status(syscall.EACCES))
			healthy := fileSnapshotRawLookup(t, raw, 1, "Healthy.strm", fuse.OK)
			fileSnapshotRawRead(t, raw, healthy.NodeId, strmTestURL(hashN(2), "Healthy.mkv")+"\n")
			backend.streamErrors = nil
			recovered := fileSnapshotRawLookup(t, raw, parent, leaf, fuse.OK)
			want := strmTestURL(hashN(1), original) + "\n"
			if recovered.Size != uint64(len(want)) {
				t.Fatalf("recovered Raw.Lookup size = %d, want %d", recovered.Size, len(want))
			}
			fileSnapshotRawRead(t, raw, recovered.NodeId, want)

			backend.streamErrors = map[string]error{errorKey: ErrPermission}
			var attr fuse.AttrOut
			if status := raw.GetAttr(nil, &fuse.GetAttrIn{InHeader: fuse.InHeader{NodeId: recovered.NodeId}}, &attr); status != fuse.Status(syscall.EACCES) {
				t.Fatalf("unavailable old inode GetAttr = %v, want EACCES", status)
			}
			if status := raw.Open(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: recovered.NodeId}}, &fuse.OpenOut{}); status != fuse.Status(syscall.EACCES) {
				t.Fatalf("unavailable old inode Open = %v, want EACCES", status)
			}
			fileSnapshotRawLookup(t, raw, parent, leaf, fuse.Status(syscall.EACCES))
			backend.streamErrors = nil
			backend.suffix = "?version=" + strings.Repeat("new", 100)
			views := strmTestClone(backend.views)
			views[0].CreatedAt = created.Add(time.Hour)
			backend.setViews(views)
			// A changed added time means the torrent was removed and added again;
			// any listing (here the root's) refreshes the cached projection.
			fileSnapshotRawLookup(t, raw, 1, "Healthy.strm", fuse.OK)
			want = strmTestURL(hashN(1), original) + backend.suffix + "\n"
			again := fileSnapshotRawLookup(t, raw, parent, leaf, fuse.OK)
			if again.NodeId != recovered.NodeId || again.Size != uint64(len(want)) {
				t.Fatalf("stable inode recovery = node %d size %d, want node %d size %d", again.NodeId, again.Size, recovered.NodeId, len(want))
			}
			if status := raw.GetAttr(nil, &fuse.GetAttrIn{InHeader: fuse.InHeader{NodeId: recovered.NodeId}}, &attr); status != fuse.OK || attr.Size != uint64(len(want)) {
				t.Fatalf("reused inode GetAttr size=%d status=%v", attr.Size, status)
			}
			assertAttrTimes(t, &attr.Attr, created.Add(time.Hour))
			fileSnapshotRawRead(t, raw, recovered.NodeId, want)
			if _, payload := backend.counts(); payload != 0 {
				t.Fatalf("metadata and generated reads opened payload %d times", payload)
			}
		})
	}
}

func TestSTRMPassthroughSnapshotUsesImmutablePayloadMetadata(t *testing.T) {
	created := time.Date(2026, 10, 20, 4, 5, 6, 0, time.UTC)
	backend := &strmTestBackend{fakeBackend: &fakeBackend{views: []TorrentView{{Name: "Collection", Hash: hashN(1), CreatedAt: created}}}}
	backend.addFile(&backend.views[0], "notes.txt", []byte("original notes\n"))
	projection := NewSTRMBackend(backend)
	stat, err := projection.(FileStatBackend).FileStat(hashN(1), "notes.txt")
	if err != nil || stat.Size != int64(len("original notes\n")) || !stat.CreatedAt.Equal(created) {
		t.Fatalf("passthrough FileStat = %+v/%v", stat, err)
	}
	if stream, payload := backend.counts(); stream != 0 || payload != 0 {
		t.Fatalf("passthrough stat called URL/payload = %d/%d, want 0/0", stream, payload)
	}
	opened, err := projection.(FileSnapshotBackend).OpenFileSnapshot(hashN(1), "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if opened.Size != stat.Size || !opened.CreatedAt.Equal(created) || strmTestRead(t, opened.Reader) != "original notes\n" {
		t.Fatalf("passthrough snapshot metadata = size %d time %v", opened.Size, opened.CreatedAt)
	}
	if stream, payload := backend.counts(); stream != 0 || payload != 1 {
		t.Fatalf("passthrough snapshot called URL/payload = %d/%d, want 0/1", stream, payload)
	}
}

func TestSTRMPayloadMetadataRejectsNonPayloadAndInvalidSizes(t *testing.T) {
	backend := &strmTestBackend{fakeBackend: &fakeBackend{views: []TorrentView{{Name: "Collection", Hash: hashN(1)}}}}
	backend.addFile(&backend.views[0], "nested/Movie.mkv", []byte("video"))
	backend.addFile(&backend.views[0], "notes.txt", []byte("notes"))
	backend.addSubtitle(&backend.views[0], "nested/Movie.srt", "nested/Movie.mkv", []byte("subtitle"))
	projection := NewSTRMBackend(backend)
	stat := projection.(FileStatBackend)
	snapshot := projection.(FileSnapshotBackend)
	for _, path := range []string{"nested", "nested/Movie.mkv", "nested/Movie.srt", "missing.strm"} {
		if _, err := stat.FileStat(hashN(1), path); !errors.Is(err, ErrNotFound) {
			t.Errorf("FileStat %q error = %v, want ErrNotFound", path, err)
		}
		if opened, err := snapshot.OpenFileSnapshot(hashN(1), path); opened.Reader != nil || !errors.Is(err, ErrNotFound) {
			t.Errorf("OpenFileSnapshot %q = %T/%v, want nil/ErrNotFound", path, opened.Reader, err)
		}
	}
	if _, err := stat.FileStat(hashN(9), "notes.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown torrent stat error = %v", err)
	}
	views := strmTestClone(backend.views)
	for i := range views[0].Files {
		views[0].Files[i].Size = -1
	}
	backend.setViews(views)
	// Payload metadata is immutable for an info hash, so a projection caches it
	// per torrent; a new mount (or a listing) observes changed metadata.
	projection = NewSTRMBackend(backend)
	stat = projection.(FileStatBackend)
	snapshot = projection.(FileSnapshotBackend)
	for _, path := range []string{"nested/Movie.strm", "notes.txt"} {
		if _, err := stat.FileStat(hashN(1), path); !errors.Is(err, ErrInvalidName) {
			t.Errorf("negative FileStat %q error = %v", path, err)
		}
		if _, err := snapshot.OpenFileSnapshot(hashN(1), path); !errors.Is(err, ErrInvalidName) {
			t.Errorf("negative OpenFileSnapshot %q error = %v", path, err)
		}
	}
	if stream, payload := backend.counts(); stream != 0 || payload != 0 {
		t.Fatalf("invalid payload requests called URL/payload = %d/%d, want 0/0", stream, payload)
	}
}
