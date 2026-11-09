package session_test

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/experiments"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// newExperimentSession builds a loopback-only session with the experiment enabled
// through the same session option a production caller uses.
func newExperimentSession(t *testing.T, torrentsDir string, settings experiments.Settings) *session.Session {
	t.Helper()
	if err := os.MkdirAll(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	cfg := testConfig()
	cfg.Connections.ListenHost = "127.0.0.1"
	sess, err := session.NewWithClientConfig(cfg, torrentsDir, func(cc *session.TorrentClientConfig) {
		cc.NoDHT = true
		cc.DisableUTP = true
		cc.DisableIPv6 = true
		cc.NoDefaultPortForwarding = true
	}, session.WithExperiment(settings))
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
	sess := newExperimentSession(t, filepath.Join(work, "torrents"), experiments.Settings{
		RuntimeSettings: experiments.RuntimeSettings{Mode: experiments.ModeInject, RateBytesPerSecond: rate, JitterFraction: 0},
		Seed:            1,
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

	// The management view reports the same synthetic counter next to the real
	// one so a tracker-side record can be compared per torrent.
	status := sess.ExperimentStatus()
	if len(status.Torrents) != 1 {
		t.Fatalf("got %d tracked torrents, want 1", len(status.Torrents))
	}
	if status.Torrents[0].InfoHash != hashHex {
		t.Fatalf("tracked hash = %q, want %q", status.Torrents[0].InfoHash, hashHex)
	}
	if status.Torrents[0].RealUploadedBytes != 0 {
		t.Fatalf("real uploaded = %d, want 0 without peers", status.Torrents[0].RealUploadedBytes)
	}
	if status.Torrents[0].ReportedUploadedBytes != last.Uploaded {
		t.Fatalf("reported uploaded = %d, want the last announced %d", status.Torrents[0].ReportedUploadedBytes, last.Uploaded)
	}
	if status.Torrents[0].RealUploadedAtLastAnnounceBytes != 0 {
		t.Fatalf("announce-time real = %d, want 0 without peers", status.Torrents[0].RealUploadedAtLastAnnounceBytes)
	}
	if status.Torrents[0].LastAnnounceAt.IsZero() {
		t.Fatal("last announce time is zero after a real announce")
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

	sess := newExperimentSession(t, filepath.Join(work, "torrents"), experiments.Settings{
		RuntimeSettings: experiments.RuntimeSettings{Mode: experiments.ModeBaseline},
	})
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

// TestExperimentSettingsHotUpdateChangesTheRunningRate proves that settings written
// through the management API take effect for later announces and are persisted.
func TestExperimentSettingsHotUpdateChangesTheRunningRate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	work := t.TempDir()
	torrentsDir := filepath.Join(work, "torrents")
	tracker := newLoopbackTracker(t)
	content := make([]byte, 16<<10)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, [][]string{{tracker.url}})
	hashHex := hash.HexString()

	sess := newExperimentSession(t, torrentsDir, experiments.Settings{
		RuntimeSettings: experiments.RuntimeSettings{Mode: experiments.ModeInject, RateBytesPerSecond: 1 << 20, JitterFraction: 0},
		Seed:            1,
	})
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: writeTorrent(t, work, torrentBytes)}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	if err := waitFor(ctx, func() bool { return tracker.announceCount(hashHex) >= 2 }); err != nil {
		t.Fatalf("tracker did not see two announces: %v", err)
	}

	before := sess.ExperimentStatus().Torrents
	if len(before) != 1 {
		t.Fatalf("got %d tracked torrents before update, want 1", len(before))
	}
	announcesBefore := tracker.announceCount(hashHex)

	applied, err := sess.SetExperimentSettings(ctx, experiments.RuntimeSettings{
		Mode:               experiments.ModeInject,
		RateBytesPerSecond: 8 << 20,
		JitterFraction:     0,
	})
	if err != nil {
		t.Fatalf("SetExperimentSettings: %v", err)
	}
	if applied.RateBytesPerSecond != 8<<20 {
		t.Fatalf("applied rate = %v, want %d", applied.RateBytesPerSecond, 8<<20)
	}
	if got := sess.ExperimentStatus().Settings.RateBytesPerSecond; got != 8<<20 {
		t.Fatalf("status rate = %v, want the applied %d", got, 8<<20)
	}

	if err := waitFor(ctx, func() bool { return tracker.announceCount(hashHex) >= announcesBefore+2 }); err != nil {
		t.Fatalf("tracker did not see announces after the update: %v", err)
	}
	after := sess.ExperimentStatus().Torrents
	if len(after) != 1 {
		t.Fatalf("got %d tracked torrents after update, want 1", len(after))
	}
	gained := after[0].ReportedUploadedBytes - before[0].ReportedUploadedBytes
	if gained < 4<<20 {
		t.Fatalf("reported upload gained %d after switching to 8MiB/s, want at least 4MiB", gained)
	}

	if _, err := os.Stat(filepath.Join(torrentsDir, ".metadata", "lab_settings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime settings wrote the retired sidecar: %v", err)
	}
}

// TestSessionIgnoresTheRetiredSettingsSidecar covers the migration boundary:
// the session no longer reads .metadata/lab_settings.json, so only the startup
// migration decides what an old sidecar means.
func TestSessionIgnoresTheRetiredSettingsSidecar(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	work := t.TempDir()
	torrentsDir := filepath.Join(work, "torrents")
	metadataDir := filepath.Join(torrentsDir, ".metadata")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("make metadata dir: %v", err)
	}
	record := `{"version":1,"mode":"amplify","rate_bytes_per_second":0,"multiplier":3,"jitter_fraction":0,"cap_bytes":0}`
	if err := os.WriteFile(filepath.Join(metadataDir, "lab_settings.json"), []byte(record), 0o600); err != nil {
		t.Fatalf("write the retired sidecar: %v", err)
	}

	sess := newExperimentSession(t, torrentsDir, experiments.Settings{
		RuntimeSettings: experiments.RuntimeSettings{Mode: experiments.ModeBaseline, JitterFraction: 0.2},
		Seed:            1,
	})
	status := sess.ExperimentStatus()
	if status.Settings.Mode != experiments.ModeBaseline {
		t.Fatalf("mode = %q, want the startup baseline; the session read the retired sidecar", status.Settings.Mode)
	}
	_ = ctx
}

func TestSetExperimentSettingsRejectsInvalidSettings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := newExperimentSession(t, t.TempDir(), experiments.Settings{
		RuntimeSettings: experiments.RuntimeSettings{Mode: experiments.ModeInject, RateBytesPerSecond: 1 << 20, JitterFraction: 0},
		Seed:            1,
	})
	if _, err := sess.SetExperimentSettings(ctx, experiments.RuntimeSettings{Mode: experiments.ModeInject}); !errors.Is(err, session.ErrInvalidExperimentSettings) {
		t.Fatalf("SetExperimentSettings(inject without rate) error = %v, want ErrInvalidExperimentSettings", err)
	}
}

// TestSessionStartsInBaselineMode covers the always-on contract: a session
// created without an experiment option still owns a reporter, and the
// management view reports the baseline settings instead of an empty one.
func TestSessionStartsInBaselineMode(t *testing.T) {
	torrentsDir := filepath.Join(t.TempDir(), "torrents")
	if err := os.MkdirAll(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	cfg := testConfig()
	cfg.Connections.ListenHost = "127.0.0.1"
	sess, err := session.NewWithClientConfig(cfg, torrentsDir, func(cc *session.TorrentClientConfig) {
		cc.NoDHT = true
		cc.DisableUTP = true
		cc.DisableIPv6 = true
		cc.NoDefaultPortForwarding = true
	})
	if err != nil {
		t.Fatalf("NewWithClientConfig: %v", err)
	}
	t.Cleanup(func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	status := sess.ExperimentStatus()
	if status.Settings.Mode != experiments.ModeBaseline {
		t.Fatalf("mode = %q, want baseline", status.Settings.Mode)
	}
	if status.Seed == 0 {
		t.Fatal("seed = 0, want a wall-clock seed")
	}
	if len(status.Torrents) != 0 {
		t.Fatalf("torrents = %+v, want an empty list before the first announce", status.Torrents)
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

	sess := newExperimentSession(t, filepath.Join(work, "torrents"), experiments.Settings{
		RuntimeSettings: experiments.RuntimeSettings{Mode: experiments.ModeBaseline},
	})
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
