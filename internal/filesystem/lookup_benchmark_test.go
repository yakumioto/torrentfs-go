package filesystem

import (
	"context"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type libraryBenchmarkBackend struct {
	views         []TorrentView
	contents      map[metainfo.Hash]TorrentContents
	torrentsCalls int
	contentsCalls int
	streamCalls   int
	payloadOpens  int
}

func (b *libraryBenchmarkBackend) Torrents() []TorrentView {
	b.torrentsCalls++
	return slices.Clone(b.views)
}

func (b *libraryBenchmarkBackend) TorrentContents(hash metainfo.Hash) (TorrentContents, error) {
	b.contentsCalls++
	contents, ok := b.contents[hash]
	if !ok {
		return TorrentContents{}, ErrNotFound
	}
	return contents, nil
}

func (b *libraryBenchmarkBackend) OpenFile(metainfo.Hash, string) (io.ReaderAt, error) {
	b.payloadOpens++
	return nil, ErrNotFound
}

func (b *libraryBenchmarkBackend) StreamURL(hash metainfo.Hash, path string) (string, error) {
	b.streamCalls++
	return "http://media.test/stream/" + hash.HexString() + "/signature/" + path, nil
}

func newLibraryBenchmarkBackend() *libraryBenchmarkBackend {
	backend := &libraryBenchmarkBackend{contents: make(map[metainfo.Hash]TorrentContents)}
	for i := 0; i < 111; i++ {
		// The baseline predates FilesID/TorrentContentsBackend; this fixture now
		// opts into exactly the immutable contract implemented by Session.
		view := TorrentView{Name: fmt.Sprintf("Torrent-%03d", i), Hash: hashN(byte(i + 1)), FilesID: &FileListID{}}
		count := 8
		if i == 0 {
			count = 18920
		} else if i > 102 {
			count = 7
		}
		view.Files = make([]FileView, count)
		for j := range view.Files {
			dir := j % 204
			view.Files[j] = FileView{Path: fmt.Sprintf("group-%02d/season-%02d/file-%05d.mkv", dir/17, dir%17, j), Size: 1}
		}
		backend.views = append(backend.views, view)
		backend.contents[view.Hash] = contentsOfView(view)
	}
	return backend
}

func BenchmarkTorrentLookup(b *testing.B) {
	for _, strm := range []bool{false, true} {
		name := "Original"
		if strm {
			name = "STRM"
		}
		b.Run(name+"/ColdDirectory", func(b *testing.B) {
			backend := newLibraryBenchmarkBackend()
			var projected Backend = backend
			if strm {
				projected = NewSTRMBackend(backend)
			}
			if _, err := torrentContents(projected, hashN(1)); err != nil {
				b.Fatal(err)
			}
			// Payload preparation and STRM projection/signing are outside the
			// timer. Each operation creates fresh mount-local state and performs
			// the first Readdir of a real deep prefix, including contents
			// validation, index construction/sorting and DirStream creation.
			// Session.New and FUSE bridge/mount setup are not measured.
			before, contentsBefore := backend.torrentsCalls, backend.contentsCalls
			streamBefore := backend.streamCalls
			ctx := context.Background()
			var builds uint64
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				state := newFSState(projected)
				dir := &torrentDirNode{state: state, hash: hashN(1), prefix: "group-00/season-00"}
				stream, errno := dir.Readdir(ctx)
				if errno != 0 {
					b.Fatal(errno)
				}
				stream.Close()
				if state.directoryBuilds != 1 {
					b.Fatalf("cold directory built %d indexes, want 1", state.directoryBuilds)
				}
				builds += state.directoryBuilds
			}
			b.StopTimer()
			b.ReportMetric(float64(backend.torrentsCalls-before)/float64(b.N), "library-lists/op")
			b.ReportMetric(float64(backend.contentsCalls-contentsBefore)/float64(b.N), "contents/op")
			b.ReportMetric(float64(builds)/float64(b.N), "directory-builds/op")
			if builds != uint64(b.N) || backend.torrentsCalls != before || backend.contentsCalls-contentsBefore != b.N || backend.streamCalls != streamBefore {
				b.Fatal("cold directory did not build exactly one index per contents read, or listed/signed the library")
			}
			if backend.payloadOpens != 0 {
				b.Fatalf("cold directory opened payload %d times", backend.payloadOpens)
			}
		})
		for _, operation := range []string{"Root", "DeepLookup", "DeepReaddir"} {
			b.Run(name+"/"+operation, func(b *testing.B) {
				backend := newLibraryBenchmarkBackend()
				var projected Backend = backend
				if strm {
					projected = NewSTRMBackend(backend)
				}
				root := &rootNode{state: newFSState(projected)}
				_ = fs.NewNodeFS(root, nil)
				ctx := context.Background()
				inode, errno := root.Lookup(ctx, "Torrent-000", &fuse.EntryOut{})
				if errno != 0 {
					b.Fatal(errno)
				}
				dir := inode.Operations().(*torrentDirNode)
				for _, part := range []string{"group-00", "season-00"} {
					inode, errno = dir.Lookup(ctx, part, &fuse.EntryOut{})
					if errno != 0 {
						b.Fatal(errno)
					}
					dir = inode.Operations().(*torrentDirNode)
				}
				leaf := "file-00000.mkv"
				if strm {
					leaf = "file-00000.strm"
				}
				// Warm the exact operation before measuring the steady state.
				if _, errno := dir.Lookup(ctx, leaf, &fuse.EntryOut{}); errno != 0 {
					b.Fatal(errno)
				}
				before, contentsBefore := backend.torrentsCalls, backend.contentsCalls
				buildsBefore := root.state.directoryBuilds
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					switch operation {
					case "Root":
						if _, errno := root.Lookup(ctx, "Torrent-000", &fuse.EntryOut{}); errno != 0 {
							b.Fatal(errno)
						}
					case "DeepLookup":
						if _, errno := dir.Lookup(ctx, leaf, &fuse.EntryOut{}); errno != 0 {
							b.Fatal(errno)
						}
					case "DeepReaddir":
						stream, errno := dir.Readdir(ctx)
						if errno != 0 {
							b.Fatal(errno)
						}
						stream.Close()
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(backend.torrentsCalls-before)/float64(b.N), "library-lists/op")
				b.ReportMetric(float64(backend.contentsCalls-contentsBefore)/float64(b.N), "contents/op")
				if operation != "Root" {
					builds := root.state.directoryBuilds - buildsBefore
					b.ReportMetric(float64(builds)/float64(b.N), "directory-builds/op")
					if builds != 0 || backend.torrentsCalls != before || backend.contentsCalls-contentsBefore != b.N {
						b.Fatal("hot directory operation rebuilt its index or listed the library")
					}
				}
				if backend.payloadOpens != 0 {
					b.Fatalf("metadata lookup opened payload %d times", backend.payloadOpens)
				}
			})
		}
	}
}

func BenchmarkSTRMProjection(b *testing.B) {
	for _, cold := range []bool{false, true} {
		name := "Hot"
		if cold {
			name = "Cold"
		}
		b.Run(name, func(b *testing.B) {
			backend := newLibraryBenchmarkBackend()
			projected := NewSTRMBackend(backend)
			projected.Torrents()
			before := backend.streamCalls
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if cold {
					projected = NewSTRMBackend(backend)
				}
				views := projected.Torrents()
				if len(views) != 111 || len(views[0].Files) != 18920 {
					b.Fatal("incomplete projected benchmark library")
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(backend.streamCalls-before)/float64(b.N), "signatures/op")
			if backend.payloadOpens != 0 {
				b.Fatalf("projection opened payload %d times", backend.payloadOpens)
			}
		})
	}
}
