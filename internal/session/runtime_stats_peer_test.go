package session_test

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

func TestRuntimeStatsLoopbackPeerPayloadAndRestart(t *testing.T) {
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

	leecherDir := filepath.Join(work, "leecher-data")
	leecher := newLivingSession(t, cfg, leecherDir, nil)
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
	got := make([]byte, len(content))
	if _, err := io.ReadFull(io.NewSectionReader(reader, 0, int64(len(got))), got); err != nil {
		t.Fatalf("leecher payload read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("leecher payload differs from seeder content")
	}

	if err := waitFor(ctx, func() bool {
		return leecher.RuntimeStats().DownloadedBytes > 0 && seeder.RuntimeStats().UploadedBytes > 0
	}); err != nil {
		t.Fatalf("peer payload counters did not grow: %v", err)
	}
	beforeRestart := leecher.RuntimeStats()
	if beforeRestart.DownloadedBytes <= 0 || beforeRestart.UploadedBytes != 0 {
		t.Fatalf("leecher stats = %+v, want positive download and no upload", beforeRestart)
	}
	seederStats := seeder.RuntimeStats()
	leecherView, err := leecher.TorrentViewFor(hashHex)
	if err != nil {
		t.Fatalf("leecher TorrentViewFor: %v", err)
	}
	if leecherView.DownloadedBytes <= 0 || leecherView.UploadedBytes != 0 {
		t.Fatalf("leecher torrent view = %+v, want positive download and no upload", leecherView)
	}
	seederView, err := seeder.TorrentViewFor(hashHex)
	if err != nil {
		t.Fatalf("seeder TorrentViewFor: %v", err)
	}
	if seederView.DownloadedBytes != 0 || seederView.UploadedBytes <= 0 {
		t.Fatalf("seeder torrent view = %+v, want no download and positive upload", seederView)
	}
	t.Logf("loopback peer payload: downloaded=%d uploaded_by_seeder=%d torrent_downloaded=%d torrent_uploaded=%d started_at=%s", beforeRestart.DownloadedBytes, seederStats.UploadedBytes, leecherView.DownloadedBytes, seederView.UploadedBytes, beforeRestart.StartedAt.Format(time.RFC3339Nano))

	closeSession(t, leecher)
	restarted := newLivingSession(t, cfg, leecherDir, nil)
	defer closeSession(t, restarted)
	afterRestart := restarted.RuntimeStats()
	if afterRestart.DownloadedBytes != 0 || afterRestart.UploadedBytes != 0 {
		t.Fatalf("restarted stats = %+v, want zero transfer counters", afterRestart)
	}
	if afterRestart.StartedAt.Equal(beforeRestart.StartedAt) {
		t.Fatalf("restarted started_at = %s, want a new Session boundary", afterRestart.StartedAt.Format(time.RFC3339Nano))
	}
	t.Logf("after restart: downloaded=%d uploaded=%d started_at=%s", afterRestart.DownloadedBytes, afterRestart.UploadedBytes, afterRestart.StartedAt.Format(time.RFC3339Nano))
}

func TestTorrentViewStatsResetAfterRestartWithoutPeers(t *testing.T) {
	work := t.TempDir()
	torrentsDir := filepath.Join(work, "data")
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", []byte("no peer payload"), nil)
	cfg := testConfig()

	sess := newLivingSession(t, cfg, torrentsDir, nil)
	if _, err := sess.AddTorrentAndPersist(context.Background(), session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("AddTorrentAndPersist: %v", err)
	}
	before, err := sess.TorrentViewFor(hash.HexString())
	if err != nil {
		t.Fatalf("TorrentViewFor before restart: %v", err)
	}
	if before.DownloadedBytes != 0 || before.UploadedBytes != 0 {
		t.Fatalf("before restart transfer counters = %d/%d, want zero", before.DownloadedBytes, before.UploadedBytes)
	}
	closeSession(t, sess)

	restarted := newLivingSession(t, cfg, torrentsDir, nil)
	defer closeSession(t, restarted)
	after, err := restarted.TorrentViewFor(hash.HexString())
	if err != nil {
		t.Fatalf("TorrentViewFor after restart: %v", err)
	}
	if after.DownloadedBytes != 0 || after.UploadedBytes != 0 {
		t.Fatalf("after restart transfer counters = %d/%d, want zero", after.DownloadedBytes, after.UploadedBytes)
	}
}
