package smb

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/sonroyaalmerol/go-smb-server/smb/vfs"
	"github.com/sonroyaalmerol/go-smb-server/smb/wire"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

type testSource struct {
	views  []filesystem.TorrentView
	reader io.ReaderAt
}

func (b *testSource) Torrents() []filesystem.TorrentView                  { return b.views }
func (b *testSource) OpenFile(metainfo.Hash, string) (io.ReaderAt, error) { return b.reader, nil }
func (b *testSource) StreamURL(metainfo.Hash, string) (string, error) {
	return "https://media.test/stream", nil
}
func sourceWith(reader io.ReaderAt, size int64) *testSource {
	return &testSource{reader: reader, views: []filesystem.TorrentView{{Hash: metainfo.Hash{1}, Name: "电影😀.txt", SingleFile: true, Files: []filesystem.FileView{{Path: "电影😀.txt", Size: size}}}}}
}
func openTestHandle(t *testing.T, b *backend) vfs.Handle {
	t.Helper()
	h, err := b.Open(context.Background(), vfs.OpenOptions{Path: "电影😀.txt", Disposition: vfs.DispositionOpen, DesiredAccess: 0x120089})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	return h
}

func TestBackendReadOnlyPathsAndEnumeration(t *testing.T) {
	ctx := context.Background()
	b := newBackend(sourceWith(strings.NewReader("hello"), 5), time.Second)
	for _, name := range []string{"../x", "x/../电影😀.txt", "x\\..\\电影😀.txt", "\\\\server\\share", "C:\\Windows", "bad\x00name"} {
		if _, err := b.Open(ctx, vfs.OpenOptions{Path: name, Disposition: vfs.DispositionOpen}); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("path %q: %v", name, err)
		}
	}
	for _, opts := range []vfs.OpenOptions{
		{Disposition: vfs.DispositionCreate}, {Disposition: vfs.DispositionSupersede}, {Disposition: vfs.DispositionOverwrite}, {Disposition: vfs.DispositionOverwriteIf},
		{Disposition: vfs.DispositionOpen, Append: true}, {Disposition: vfs.DispositionOpen, CreateOptions: wire.FileDeleteOnClose},
		{Disposition: vfs.DispositionOpen, DesiredAccess: 2}, {Disposition: vfs.DispositionOpen, DesiredAccess: 4},
		{Disposition: vfs.DispositionOpen, DesiredAccess: 0x10000}, {Disposition: vfs.DispositionOpen, DesiredAccess: 0x40000000},
		{Disposition: vfs.DispositionOpen, DesiredAccess: 0x10000000}, {Disposition: vfs.DispositionOpen, DesiredAccess: 0x100},
	} {
		opts.Path = "电影😀.txt"
		if _, err := b.Open(ctx, opts); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("mutation %+v: %v", opts, err)
		}
	}
	if _, err := b.Open(ctx, vfs.OpenOptions{Path: "missing", Disposition: vfs.DispositionOpenIf}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("OPEN_IF missing: %v", err)
	}
	if _, err := b.Open(ctx, vfs.OpenOptions{Path: "电影😀.txt", Disposition: vfs.DispositionOpen, CreateDir: true}); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("directory constraint: %v", err)
	}
	dir, err := b.Open(ctx, vfs.OpenOptions{Disposition: vfs.DispositionOpen, CreateDir: true})
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close(ctx)
	for _, pattern := range []string{"*", "*.*", "*.TXT"} {
		count := 0
		for fi, err := range dir.Enumerate(ctx, pattern) {
			if err != nil {
				t.Fatal(err)
			}
			count++
			if fi.Name != "电影😀.txt" {
				t.Fatal(fi)
			}
		}
		if count != 1 {
			t.Fatalf("pattern %q: %d", pattern, count)
		}
	}
	h := openTestHandle(t, b)
	for _, err := range []error{b.Remove(ctx, "电影😀.txt"), b.Mkdir(ctx, "new"), h.(vfs.SetInfoer).SetInfo(ctx, &vfs.SetInfoRequest{}), h.(vfs.Renamer).Rename(ctx, "new", false)} {
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("mutation: %v", err)
		}
	}
	if _, err := h.Write(ctx, 0, []byte("x")); !errors.Is(err, fs.ErrPermission) {
		t.Fatal(err)
	}
	p := make([]byte, 8)
	if n, err := h.Read(ctx, 0, p); err != nil || n != 5 || string(p[:n]) != "hello" {
		t.Fatalf("read: %d %q %v", n, p, err)
	}
	if n, err := h.Read(ctx, 5, p); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("EOF: %d %v", n, err)
	}
}

type blockingReader struct {
	started chan struct{}
	closed  atomic.Int32
}

func (b *blockingReader) ReadAt([]byte, int64) (int, error) { panic("context not forwarded") }
func (b *blockingReader) ReadAtContext(ctx context.Context, _ []byte, _ int64) (int, error) {
	close(b.started)
	<-ctx.Done()
	return 0, ctx.Err()
}
func (b *blockingReader) Close() error { b.closed.Add(1); return nil }

func TestReadCancellationTimeoutAndIdempotentClose(t *testing.T) {
	for _, reason := range []string{"timeout", "handle", "service"} {
		t.Run(reason, func(t *testing.T) {
			r := &blockingReader{started: make(chan struct{})}
			b := newBackend(sourceWith(r, 5), 20*time.Millisecond)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h, err := b.Open(ctx, vfs.OpenOptions{Path: "电影😀.txt", Disposition: vfs.DispositionOpen})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := h.Read(context.Background(), 0, make([]byte, 5)); done <- err }()
			<-r.started
			switch reason {
			case "handle":
				_ = h.Close(context.Background())
			case "service":
				cancel()
			}
			select {
			case err := <-done:
				if reason == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("timeout: %v", err)
				}
				if reason != "timeout" && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("read did not cancel")
			}
			_ = h.Close(context.Background())
			_ = h.Close(context.Background())
			if r.closed.Load() != 1 {
				t.Fatalf("close count: %d", r.closed.Load())
			}
			if _, err := h.Read(context.Background(), 0, nil); !errors.Is(err, fs.ErrClosed) {
				t.Fatalf("closed read: %v", err)
			}
		})
	}
	b := newBackend(sourceWith(strings.NewReader("short"), 10), time.Second)
	if _, err := openTestHandle(t, b).Read(context.Background(), 0, make([]byte, 10)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short read: %v", err)
	}
}

type largeReader struct{ offset int64 }

func (r *largeReader) ReadAt(p []byte, off int64) (int, error) {
	r.offset = off
	for i := range p {
		p[i] = byte(off + int64(i))
	}
	return len(p), nil
}
func TestReadLargeOffset(t *testing.T) {
	r := &largeReader{}
	b := newBackend(sourceWith(r, 8<<30), time.Second)
	p := make([]byte, 123)
	offset := int64(5<<30) + 77
	if n, err := openTestHandle(t, b).Read(context.Background(), offset, p); n != len(p) || err != nil || r.offset != offset {
		t.Fatalf("large offset: %d %v %d", n, err, r.offset)
	}
}
