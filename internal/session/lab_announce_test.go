package session_test

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/lab"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// newLabSession builds a loopback-only session with the requested experiment
// reporter installed through the same option a production caller uses.
func newLabSession(t *testing.T, torrentsDir string, settings lab.Settings) *session.Session {
	t.Helper()
	if err := os.MkdirAll(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	cfg := testConfig()
	cfg.Connections.ListenHost = "127.0.0.1"
	reporter := lab.NewReporter(settings, nil)
	sess, err := session.NewWithClientConfig(cfg, torrentsDir, func(cc *session.TorrentClientConfig) {
		cc.NoDHT = true
		cc.DisableUTP = true
		cc.DisableIPv6 = true
		cc.NoDefaultPortForwarding = true
	}, session.WithAnnounceRequestDirector(reporter.Director))
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

func writeTorrent(t *testing.T, work string, torrentBytes []byte) string {
	t.Helper()
	path := filepath.Join(work, "payload.torrent")
	if err := os.WriteFile(path, torrentBytes, 0o644); err != nil {
		t.Fatalf("write torrent: %v", err)
	}
	return path
}

// TestSyntheticAnnounceUploadReachesTracker drives a real client against a
// loopback HTTP tracker and proves that inject mode replaces the announce
// "uploaded" parameter with synthetic growth while downloaded and left keep
// their real values.
func TestSyntheticAnnounceUploadReachesTracker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	work := t.TempDir()
	tracker := newLoopbackTracker(t)
	content := make([]byte, 64<<10)
	for i := range content {
		content[i] = byte(i*17 + i/251)
	}
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, [][]string{{tracker.url}})
	hashHex := hash.HexString()

	const rate = 1 << 20 // 1 MiB/s
	sess := newLabSession(t, filepath.Join(work, "torrents"), lab.Settings{
		Mode:               lab.ModeInject,
		RateBytesPerSecond: rate,
		JitterFraction:     0,
		Seed:               1,
	})
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: writeTorrent(t, work, torrentBytes)}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	if err := waitFor(ctx, func() bool { return tracker.announceCount(hashHex) >= 3 }); err != nil {
		t.Fatalf("tracker did not see three announces: %v", err)
	}

	announces := tracker.announceSnapshot(hashHex)
	if len(announces) < 3 {
		t.Fatalf("tracker recorded %d announces, want at least 3", len(announces))
	}
	if got := announces[0].Uploaded; got != 0 {
		t.Fatalf("first announce reports uploaded = %d, want the 0 of a started event", got)
	}
	previous := int64(0)
	for i, announce := range announces {
		if announce.Uploaded < previous {
			t.Fatalf("announce %d dropped uploaded from %d to %d", i, previous, announce.Uploaded)
		}
		previous = announce.Uploaded
	}

	first := announces[0]
	last := announces[len(announces)-1]
	if last.Uploaded <= 0 {
		t.Fatalf("last announce reports uploaded = 0, want synthetic growth")
	}
	elapsed := last.At.Sub(first.At).Seconds()
	want := float64(rate) * elapsed
	if math.Abs(float64(last.Uploaded)-want) > 0.25*want+float64(rate) {
		t.Fatalf("last uploaded = %d, want about %.0f for %.2fs at %d B/s", last.Uploaded, want, elapsed, rate)
	}

	// Only "uploaded" is synthetic. The torrent has no peers, so the real
	// downloaded counter stays zero and left keeps the full payload length.
	if got := last.Downloaded; got != 0 {
		t.Fatalf("last announce downloaded = %d, want the real 0", got)
	}
	if want := int64(len(content)); last.Left != want {
		t.Fatalf("last announce left = %d, want the real %d", last.Left, want)
	}
}

// TestBaselineAnnounceKeepsRealUpload is the control arm: the same client and
// tracker, but baseline mode reports the real payload counters.
func TestBaselineAnnounceKeepsRealUpload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	work := t.TempDir()
	tracker := newLoopbackTracker(t)
	content := make([]byte, 16<<10)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, [][]string{{tracker.url}})
	hashHex := hash.HexString()

	sess := newLabSession(t, filepath.Join(work, "torrents"), lab.Settings{Mode: lab.ModeBaseline})
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: writeTorrent(t, work, torrentBytes)}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	if err := waitFor(ctx, func() bool { return tracker.announceCount(hashHex) >= 2 }); err != nil {
		t.Fatalf("tracker did not see two announces: %v", err)
	}

	for i, announce := range tracker.announceSnapshot(hashHex) {
		if announce.Uploaded != 0 {
			t.Fatalf("baseline announce %d reports uploaded = %d, want the real 0", i, announce.Uploaded)
		}
		if announce.Left != int64(len(content)) {
			t.Fatalf("baseline announce %d left = %d, want %d", i, announce.Left, len(content))
		}
	}
}

// TestTorrentPeerActivitiesReportsRealCounters covers the sampler source: the
// peer gauges and real payload counters exposed by the session.
func TestTorrentPeerActivitiesReportsRealCounters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	work := t.TempDir()
	tracker := newLoopbackTracker(t)
	content := make([]byte, 8<<10)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, [][]string{{tracker.url}})

	sess := newLabSession(t, filepath.Join(work, "torrents"), lab.Settings{Mode: lab.ModeBaseline})
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: writeTorrent(t, work, torrentBytes)}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	if err := waitFor(ctx, func() bool { return tracker.announceCount(hash.HexString()) >= 1 }); err != nil {
		t.Fatalf("tracker did not see an announce: %v", err)
	}

	activities := sess.TorrentPeerActivities()
	if len(activities) != 1 {
		t.Fatalf("got %d activities, want 1", len(activities))
	}
	activity := activities[0]
	if activity.InfoHash != hash.HexString() {
		t.Fatalf("info hash = %q, want %q", activity.InfoHash, hash.HexString())
	}
	if activity.UploadedBytes != 0 || activity.DownloadedBytes != 0 {
		t.Fatalf("real counters = %d/%d, want 0/0 without peers", activity.UploadedBytes, activity.DownloadedBytes)
	}
	if activity.ActivePeers != 0 || activity.TotalPeers < 0 {
		t.Fatalf("peer gauges = %+v, want non-negative totals and no active peers", activity)
	}
}
