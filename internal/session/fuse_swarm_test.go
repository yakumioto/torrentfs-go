package session_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// newLoopbackSession creates a session whose client is confined to loopback:
// no DHT, no uTP, no port forwarding. The session is closed automatically.
func newLoopbackSession(t *testing.T, cfg config.Config) *session.Session {
	return newLoopbackSessionWithCustomize(t, cfg, nil)
}

func newLoopbackSessionWithCustomize(t *testing.T, cfg config.Config, customize func(*session.TorrentClientConfig)) *session.Session {
	t.Helper()
	cfg.Connections.ListenHost = "127.0.0.1"
	torrentsDir := filepath.Join(cfg.Paths.DataDir, "torrents")
	if err := os.MkdirAll(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	sess, err := session.NewWithClientConfig(cfg, torrentsDir, func(cc *session.TorrentClientConfig) {
		cc.NoDHT = true
		cc.DisableUTP = true
		cc.DisableIPv6 = true
		cc.NoDefaultPortForwarding = true
		if customize != nil {
			customize(cc)
		}
	})
	if err != nil {
		t.Fatalf("NewWithClientConfig: %v", err)
	}
	t.Cleanup(func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return sess
}

// TestFuseSwarmStreamsFromSeeder runs a real self-hosted swarm end to end: a
// seeder that already holds the data and a leecher with an empty data
// directory discover each other through a loopback HTTP tracker, and the
// leecher streams the content through a real FUSE mount. It skips (or fails
// under TORRENTFS_FUSE_REQUIRED=1) where FUSE is unavailable.
func TestFuseSwarmStreamsFromSeeder(t *testing.T) {
	requireFuse(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	work := t.TempDir()
	tracker := newLoopbackTracker(t)

	content := make([]byte, 3*testPieceLength+4096)
	for i := range content {
		content[i] = byte(i*31 + i/251)
	}
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, [][]string{{tracker.url}})
	hashHex := hash.HexString()

	// Seeder: its in-memory cache is filled directly, standing in for data that
	// would otherwise have to be downloaded first.
	seederDir := filepath.Join(work, "seeder-data")
	seederTorrent := filepath.Join(work, "seeder.torrent")
	if err := os.WriteFile(seederTorrent, torrentBytes, 0o644); err != nil {
		t.Fatalf("write seeder torrent: %v", err)
	}

	seeder := newLoopbackSession(t, testConfig(seederDir))
	if err := seeder.AddTorrent(ctx, session.Source{MetainfoPath: seederTorrent}); err != nil {
		t.Fatalf("seeder AddTorrent: %v", err)
	}
	seederHandle, ok := seeder.Torrent(hash)
	if !ok {
		t.Fatal("seeder did not register the torrent")
	}
	seedPieces(t, seeder, hash, content)
	waitCached(t, ctx, seederHandle)

	// Leecher: an empty data directory that must fetch everything.
	leecherDir := filepath.Join(work, "leecher-data")
	leecherTorrent := filepath.Join(work, "leecher.torrent")
	if err := os.WriteFile(leecherTorrent, torrentBytes, 0o644); err != nil {
		t.Fatalf("write leecher torrent: %v", err)
	}
	leecher := newLoopbackSession(t, testConfig(leecherDir))
	if err := leecher.AddTorrent(ctx, session.Source{MetainfoPath: leecherTorrent}); err != nil {
		t.Fatalf("leecher AddTorrent: %v", err)
	}
	leecherHandle, ok := leecher.Torrent(hash)
	if !ok {
		t.Fatal("leecher did not register the torrent")
	}
	if got := leecherHandle.CachedBytes(); got != 0 {
		t.Fatalf("leecher starts with %d cached bytes; the swarm test must transfer everything", got)
	}

	// Both peers must reach the tracker before the mount can expect content.
	if err := waitFor(ctx, func() bool { return tracker.announceCount(hashHex) >= 2 }); err != nil {
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

	path := filepath.Join(mnt, "payload.bin")
	got := readFileWithin(t, ctx, path)
	if !bytes.Equal(got, content) {
		t.Fatalf("streamed content differs: got %d bytes, want %d", len(got), len(content))
	}

	// A seeked read must return the same window as the source.
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	const windowOffset = 2*testPieceLength + 123
	window := make([]byte, 8192)
	if _, err := f.ReadAt(window, windowOffset); err != nil && err != io.EOF {
		t.Fatalf("ReadAt(%d): %v", windowOffset, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close mounted file: %v", err)
	}
	if !bytes.Equal(window, content[windowOffset:windowOffset+len(window)]) {
		t.Fatal("seeked read window differs from source")
	}

	waitCached(t, ctx, leecherHandle)
	if got := leecherHandle.CachedBytes(); got != leecherHandle.Length() {
		t.Fatalf("leecher cached %d/%d bytes after the mount", got, leecherHandle.Length())
	}
}

// readFileWithin reads path, failing the test if the read outlives ctx so a
// stalled download cannot hang the suite forever.
func readFileWithin(t *testing.T, ctx context.Context, path string) []byte {
	t.Helper()
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := os.ReadFile(path)
		done <- result{data: data, err: err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("ReadFile(%s): %v", path, r.err)
		}
		return r.data
	case <-ctx.Done():
		t.Fatalf("timed out reading %s: %v", path, ctx.Err())
		return nil
	}
}
