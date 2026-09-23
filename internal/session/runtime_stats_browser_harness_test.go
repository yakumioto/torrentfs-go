package session_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// TestRuntimeStatsBrowserHarness is opt-in so the normal suite stays independent
// of a local browser. It holds a real peer-backed API/UI session open while a
// browser refreshes the embedded Dashboard.
func TestRuntimeStatsBrowserHarness(t *testing.T) {
	if os.Getenv("TORRENTFS_RUNTIME_STATS_BROWSER_HARNESS") != "1" {
		t.Skip("set TORRENTFS_RUNTIME_STATS_BROWSER_HARNESS=1 for browser acceptance")
	}
	donePath := os.Getenv("TORRENTFS_RUNTIME_STATS_BROWSER_DONE")
	if donePath == "" {
		t.Fatal("TORRENTFS_RUNTIME_STATS_BROWSER_DONE is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	work := t.TempDir()
	tracker := newLoopbackTracker(t)
	content := make([]byte, 2*testPieceLength+8192)
	for i := range content {
		content[i] = byte(i*31 + i/251)
	}
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, [][]string{{tracker.url}})
	hashHex := hash.HexString()
	cfg := testConfig()

	seeder := newLivingSession(t, cfg, filepath.Join(work, "seeder-data"), nil)
	defer closeSession(t, seeder)
	if err := seeder.AddTorrent(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("seeder AddTorrent: %v", err)
	}
	seederHandle, ok := seeder.Torrent(hash)
	if !ok {
		t.Fatal("seeder did not register torrent")
	}
	seedPieces(t, seeder, hash, content)
	waitCached(t, ctx, seederHandle)

	leecher := newLivingSession(t, cfg, filepath.Join(work, "leecher-data"), nil)
	defer closeSession(t, leecher)
	if err := leecher.AddTorrent(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("leecher AddTorrent: %v", err)
	}
	if err := waitFor(ctx, func() bool { return tracker.peerCount(hashHex) >= 2 }); err != nil {
		t.Fatalf("seeder and leecher never connected: %v", err)
	}
	reader, err := leecher.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatalf("leecher OpenFile: %v", err)
	}
	if _, err := reader.ReadAt(make([]byte, len(content)), 0); err != nil {
		t.Fatalf("leecher payload read: %v", err)
	}
	if err := waitFor(ctx, func() bool {
		return leecher.RuntimeStats().DownloadedBytes > 0 && seeder.RuntimeStats().UploadedBytes > 0
	}); err != nil {
		t.Fatalf("peer payload counters did not grow: %v", err)
	}

	apiServer, err := api.New(cfg, leecher)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	serveCtx, stopServe := context.WithCancel(context.Background())
	defer func() {
		stopServe()
		_ = apiServer.Shutdown(context.Background())
	}()
	serveErr := make(chan error, 1)
	go func() { serveErr <- apiServer.Serve(serveCtx, "127.0.0.1:18081") }()
	if err := waitFor(ctx, func() bool {
		response, err := http.Get("http://127.0.0.1:18081/api/v1/stats")
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	}); err != nil {
		t.Fatalf("API server never became ready: %v", err)
	}
	stats := leecher.RuntimeStats()
	seederStats := seeder.RuntimeStats()
	t.Logf("BROWSER_HARNESS_READY url=http://127.0.0.1:18081/ downloaded=%d uploaded_by_seeder=%d started_at=%s", stats.DownloadedBytes, seederStats.UploadedBytes, stats.StartedAt.Format(time.RFC3339Nano))

	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(donePath); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("browser harness context ended: %v", ctx.Err())
		case <-deadline.C:
			t.Fatal("browser did not signal completion")
		case <-time.After(100 * time.Millisecond):
		}
		select {
		case err := <-serveErr:
			if err != nil {
				t.Fatalf("API server stopped during browser harness: %v", err)
			}
		default:
		}
	}
}
