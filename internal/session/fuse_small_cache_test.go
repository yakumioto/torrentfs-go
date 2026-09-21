package session_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// TestFuseStreamsFileLargerThanCache streams a multi-piece torrent through a
// real FUSE mount on a leecher whose piece cache is smaller than the file, so
// pieces are evicted while the reader is still consuming the file. Every byte
// must still match the source: a partially received piece must never be served
// as data, which would surface as zeros or stale bytes in the mounted file.
func TestFuseStreamsFileLargerThanCache(t *testing.T) {
	requireFuse(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	work := t.TempDir()
	tracker := newLoopbackTracker(t)

	content := make([]byte, 4*testPieceLength+777)
	for i := range content {
		content[i] = byte(i*37 + i/191)
	}
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, [][]string{{tracker.url}})

	seederDir := filepath.Join(work, "seeder-data")
	seederTorrent := filepath.Join(work, "seeder.torrent")
	if err := os.WriteFile(seederTorrent, torrentBytes, 0o644); err != nil {
		t.Fatalf("write seeder torrent: %v", err)
	}
	seeder := newLoopbackSession(t, testConfig(), seederDir)
	if err := seeder.AddTorrent(ctx, session.Source{MetainfoPath: seederTorrent}); err != nil {
		t.Fatalf("seeder AddTorrent: %v", err)
	}
	seederHandle, ok := seeder.Torrent(hash)
	if !ok {
		t.Fatal("seeder did not register the torrent")
	}
	seedPieces(t, seeder, hash, content)
	waitCached(t, ctx, seederHandle)

	// The leecher's cache holds two pieces at most, so a four-piece file cannot
	// stay resident while it is read.
	cfg := testConfig()
	cfg.Cache.CapacityBytes = 2 * testPieceLength
	if cfg.Cache.CapacityBytes >= int64(len(content)) {
		t.Fatalf("test cache %d must be smaller than the %d byte file", cfg.Cache.CapacityBytes, len(content))
	}
	leecherDir := filepath.Join(work, "leecher-data")
	leecherTorrent := filepath.Join(work, "leecher.torrent")
	if err := os.WriteFile(leecherTorrent, torrentBytes, 0o644); err != nil {
		t.Fatalf("write leecher torrent: %v", err)
	}
	leecher := newLoopbackSession(t, cfg, leecherDir)
	if err := leecher.AddTorrent(ctx, session.Source{MetainfoPath: leecherTorrent}); err != nil {
		t.Fatalf("leecher AddTorrent: %v", err)
	}
	if _, ok := leecher.Torrent(hash); !ok {
		t.Fatal("leecher did not register the torrent")
	}

	if err := waitFor(ctx, func() bool { return tracker.announceCount(hash.HexString()) >= 2 }); err != nil {
		t.Fatalf("tracker never saw both peers announce: %v", err)
	}

	mnt := filepath.Join(work, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatalf("make mountpoint: %v", err)
	}
	server, err := filesystem.Mount(mnt, leecher, nil)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(func() { unmountServer(t, server, mnt) })

	got := readFileWithin(t, ctx, filepath.Join(mnt, "payload.bin"))
	if len(got) != len(content) {
		t.Fatalf("read %d bytes through the mount, want %d", len(got), len(content))
	}
	if !bytes.Equal(got, content) {
		first := 0
		for first < len(content) && got[first] == content[first] {
			first++
		}
		t.Fatalf("mounted content differs from the source at byte %d (piece %d)",
			first, int64(first)/int64(testPieceLength))
	}
}
