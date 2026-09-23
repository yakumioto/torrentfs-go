package session_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

func TestSessionRuntimeStatsUsesSharedCacheSnapshot(t *testing.T) {
	work := t.TempDir()
	content := []byte("runtime stats cache payload")
	torrentPath, hash := buildSingleFileTorrent(t, work, "payload.bin", content)
	cfg := testConfig()
	sess, err := session.New(cfg, testTorrentDir(t, filepath.Join(work, "data")))
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	if err := sess.AddTorrent(context.Background(), session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatalf("torrent %s was not registered", hash)
	}
	seedPieces(t, sess, hash, content)
	waitCached(t, context.Background(), st)

	got := sess.RuntimeStats()
	if got.CacheUsedBytes != int64(len(content)) {
		t.Fatalf("CacheUsedBytes = %d, want %d", got.CacheUsedBytes, len(content))
	}
	if got.CacheCapacityBytes != cfg.Cache.CapacityBytes {
		t.Fatalf("CacheCapacityBytes = %d, want %d", got.CacheCapacityBytes, cfg.Cache.CapacityBytes)
	}
	if got.DownloadedBytes != 0 || got.UploadedBytes != 0 {
		t.Fatalf("transfer = (%d, %d), want zero for a seeded offline session", got.DownloadedBytes, got.UploadedBytes)
	}
	if got.StartedAt.IsZero() {
		t.Fatal("StartedAt is zero")
	}
}
