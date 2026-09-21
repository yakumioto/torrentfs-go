package session

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"

	"github.com/yakumioto/torrentfs-go/internal/cache"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

const largeOffsetPieceLength = int64(1 << 20)

type largeOffsetFuseProbe struct{ fs.Inode }

type largeOffsetSource struct {
	mu          sync.Mutex
	pieceLength int64
	offsets     []int64
	maxRead     int
}

func (s *largeOffsetSource) ReadAtContext(ctx context.Context, dst []byte, off, _ int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, syscall.EINVAL
	}
	s.mu.Lock()
	s.offsets = append(s.offsets, off)
	if len(dst) > s.maxRead {
		s.maxRead = len(dst)
	}
	s.mu.Unlock()
	for i := range dst {
		dst[i] = byte((off + int64(i)) % 251)
	}
	return len(dst), nil
}

func (s *largeOffsetSource) Close() error { return nil }

type largeOffsetBackend struct {
	hash metainfo.Hash
	file *raFile
	size int64
}

func (b *largeOffsetBackend) Torrents() []filesystem.TorrentView {
	return []filesystem.TorrentView{{
		Name: "payload.bin",
		Hash: b.hash,
		Files: []filesystem.FileView{{
			Path: "payload.bin",
			Size: b.size,
		}},
		SingleFile: true,
	}}
}

func (b *largeOffsetBackend) OpenFile(hash metainfo.Hash, path string) (io.ReaderAt, error) {
	if hash != b.hash || path != "payload.bin" {
		return nil, os.ErrNotExist
	}
	return b.file, nil
}

func requireInternalFuse(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		if os.Getenv("TORRENTFS_FUSE_REQUIRED") == "1" {
			t.Fatalf("TORRENTFS_FUSE_REQUIRED=1 but /dev/fuse is unavailable: %v", err)
		}
		t.Skip("FUSE is unavailable: /dev/fuse is missing")
	}
	mountpoint := t.TempDir()
	server, err := fs.Mount(mountpoint, &largeOffsetFuseProbe{}, nil)
	if err != nil {
		if os.Getenv("TORRENTFS_FUSE_REQUIRED") == "1" {
			t.Fatalf("TORRENTFS_FUSE_REQUIRED=1 but FUSE mount failed: %v", err)
		}
		t.Skipf("FUSE mount is unavailable: %v", err)
	}
	if err := server.Unmount(); err != nil {
		t.Fatalf("unmount FUSE probe: %v", err)
	}
}

func TestFuseLargeOffsetReadAt(t *testing.T) {
	requireInternalFuse(t)

	const fileSize = int64(4<<30) + 3*largeOffsetPieceLength + 123
	var hash metainfo.Hash
	hash[0] = 1
	source := &largeOffsetSource{pieceLength: largeOffsetPieceLength}
	file := &raFile{
		loader:      source,
		cache:       cache.New(2 * largeOffsetPieceLength),
		torrentKey:  hash.HexString(),
		fileSize:    fileSize,
		pieceLength: largeOffsetPieceLength,
		torrentSize: fileSize,
		readahead:   largeOffsetPieceLength,
	}
	backend := &largeOffsetBackend{hash: hash, file: file, size: fileSize}

	mountpoint := filepath.Join(t.TempDir(), "mnt")
	if err := os.Mkdir(mountpoint, 0o755); err != nil {
		t.Fatalf("create mountpoint: %v", err)
	}
	server, err := filesystem.Mount(mountpoint, backend, &fs.Options{
		UID: uint32(os.Getuid()),
		GID: uint32(os.Getgid()),
	})
	if err != nil {
		t.Fatalf("mount large-offset backend: %v", err)
	}
	defer func() { _ = server.Unmount() }()

	path := filepath.Join(mountpoint, "payload.bin")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat large file: %v", err)
	}
	if got := info.Size(); got != fileSize {
		t.Fatalf("file size = %d, want %d", got, fileSize)
	}
	opened, err := os.Open(path)
	if err != nil {
		t.Fatalf("open large file: %v", err)
	}

	readAt := func(offset int64, length int) {
		t.Helper()
		want := make([]byte, length)
		for i := range want {
			want[i] = byte((offset + int64(i)) % 251)
		}
		got := make([]byte, length)
		n, err := opened.ReadAt(got, offset)
		if err != nil {
			t.Fatalf("ReadAt(%d, %d): n=%d err=%v", offset, length, n, err)
		}
		if n != length {
			t.Fatalf("ReadAt(%d, %d): n=%d, want %d", offset, length, n, length)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ReadAt(%d, %d) byte %d = %d, want %d", offset, length, i, got[i], want[i])
			}
		}
	}

	readAt(2<<30+37, 73)
	readAt(4<<30+2*largeOffsetPieceLength+19, 97)
	if _, err := opened.Seek(4<<30+largeOffsetPieceLength+41, io.SeekStart); err != nil {
		t.Fatalf("seek to large offset: %v", err)
	}
	seeked := make([]byte, 89)
	if _, err := io.ReadFull(opened, seeked); err != nil {
		t.Fatalf("read after large seek: %v", err)
	}
	for i, got := range seeked {
		want := byte((4<<30 + largeOffsetPieceLength + 41 + int64(i)) % 251)
		if got != want {
			t.Fatalf("seeked byte %d = %d, want %d", i, got, want)
		}
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("close large file: %v", err)
	}
	if err := server.Unmount(); err != nil {
		t.Fatalf("unmount large-offset backend: %v", err)
	}

	source.mu.Lock()
	defer source.mu.Unlock()
	for _, offset := range []int64{
		(2 << 30) / largeOffsetPieceLength * largeOffsetPieceLength,
		(4<<30 + 2*largeOffsetPieceLength + 19) / largeOffsetPieceLength * largeOffsetPieceLength,
		(4<<30 + largeOffsetPieceLength + 41) / largeOffsetPieceLength * largeOffsetPieceLength,
	} {
		found := false
		for _, got := range source.offsets {
			if got == offset {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("piece source offsets = %v, want piece offset %d", source.offsets, offset)
		}
	}
	if source.maxRead > int(largeOffsetPieceLength) {
		t.Fatalf("piece source materialized %d bytes, want at most one piece (%d)", source.maxRead, largeOffsetPieceLength)
	}
}
