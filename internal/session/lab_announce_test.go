package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/lab"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// newLabSession builds a loopback-only session with the experiment enabled
// through the same session option a production caller uses.
func newLabSession(t *testing.T, torrentsDir string, settings lab.Settings) *session.Session {
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
	}, session.WithLabExperiment(settings))
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
		RuntimeSettings: lab.RuntimeSettings{Mode: lab.ModeInject, RateBytesPerSecond: rate, JitterFraction: 0},
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
	status := sess.LabStatus()
	if !status.Enabled {
		t.Fatal("LabStatus reports the experiment disabled")
	}
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

	sess := newLabSession(t, filepath.Join(work, "torrents"), lab.Settings{
		RuntimeSettings: lab.RuntimeSettings{Mode: lab.ModeBaseline},
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

// TestLabSettingsHotUpdateChangesTheRunningRate proves that settings written
// through the management API take effect for later announces and are persisted.
func TestLabSettingsHotUpdateChangesTheRunningRate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	work := t.TempDir()
	torrentsDir := filepath.Join(work, "torrents")
	tracker := newLoopbackTracker(t)
	content := make([]byte, 16<<10)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, [][]string{{tracker.url}})
	hashHex := hash.HexString()

	sess := newLabSession(t, torrentsDir, lab.Settings{
		RuntimeSettings: lab.RuntimeSettings{Mode: lab.ModeInject, RateBytesPerSecond: 1 << 20, JitterFraction: 0},
		Seed:            1,
	})
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: writeTorrent(t, work, torrentBytes)}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	if err := waitFor(ctx, func() bool { return tracker.announceCount(hashHex) >= 2 }); err != nil {
		t.Fatalf("tracker did not see two announces: %v", err)
	}

	before := sess.LabStatus().Torrents
	if len(before) != 1 {
		t.Fatalf("got %d tracked torrents before update, want 1", len(before))
	}
	announcesBefore := tracker.announceCount(hashHex)

	applied, err := sess.SetLabSettings(ctx, lab.RuntimeSettings{
		Mode:               lab.ModeInject,
		RateBytesPerSecond: 8 << 20,
		JitterFraction:     0,
	})
	if err != nil {
		t.Fatalf("SetLabSettings: %v", err)
	}
	if applied.RateBytesPerSecond != 8<<20 {
		t.Fatalf("applied rate = %v, want %d", applied.RateBytesPerSecond, 8<<20)
	}
	if got := sess.LabStatus().Settings.RateBytesPerSecond; got != 8<<20 {
		t.Fatalf("status rate = %v, want the applied %d", got, 8<<20)
	}

	if err := waitFor(ctx, func() bool { return tracker.announceCount(hashHex) >= announcesBefore+2 }); err != nil {
		t.Fatalf("tracker did not see announces after the update: %v", err)
	}
	after := sess.LabStatus().Torrents
	if len(after) != 1 {
		t.Fatalf("got %d tracked torrents after update, want 1", len(after))
	}
	gained := after[0].ReportedUploadedBytes - before[0].ReportedUploadedBytes
	if gained < 4<<20 {
		t.Fatalf("reported upload gained %d after switching to 8MiB/s, want at least 4MiB", gained)
	}

	persisted, err := os.ReadFile(filepath.Join(torrentsDir, ".metadata", "lab_settings.json"))
	if err != nil {
		t.Fatalf("read persisted lab settings: %v", err)
	}
	var record struct {
		Mode               string  `json:"mode"`
		RateBytesPerSecond float64 `json:"rate_bytes_per_second"`
	}
	if err := json.Unmarshal(persisted, &record); err != nil {
		t.Fatalf("decode persisted lab settings: %v", err)
	}
	if record.Mode != string(lab.ModeInject) || record.RateBytesPerSecond != 8<<20 {
		t.Fatalf("persisted settings = %+v, want inject at 8MiB/s", record)
	}
}

// TestLabSettingsRestoreAfterRestart covers the sidecar: a runtime update is
// restored by the next session even though the environment still carries the
// startup values.
func TestLabSettingsRestoreAfterRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	work := t.TempDir()
	torrentsDir := filepath.Join(work, "torrents")
	startup := lab.Settings{
		RuntimeSettings: lab.RuntimeSettings{Mode: lab.ModeInject, RateBytesPerSecond: 1 << 20, JitterFraction: 0},
		Seed:            1,
	}

	first := newLabSession(t, torrentsDir, startup)
	if _, err := first.SetLabSettings(ctx, lab.RuntimeSettings{
		Mode:           lab.ModeAmplify,
		Multiplier:     3,
		JitterFraction: 0,
	}); err != nil {
		t.Fatalf("SetLabSettings: %v", err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := newLabSession(t, torrentsDir, startup)
	status := second.LabStatus()
	if !status.Enabled {
		t.Fatal("restored session reports the experiment disabled")
	}
	if status.Settings.Mode != lab.ModeAmplify || status.Settings.Multiplier != 3 {
		t.Fatalf("restored settings = %+v, want amplify x3", status.Settings)
	}
}

func TestSetLabSettingsRejectsInvalidAndDisabled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	torrentsDir := t.TempDir()
	sess := newLabSession(t, torrentsDir, lab.Settings{
		RuntimeSettings: lab.RuntimeSettings{Mode: lab.ModeInject, RateBytesPerSecond: 1 << 20, JitterFraction: 0},
		Seed:            1,
	})
	if _, err := sess.SetLabSettings(ctx, lab.RuntimeSettings{Mode: lab.ModeInject}); !errors.Is(err, session.ErrInvalidLabSettings) {
		t.Fatalf("SetLabSettings(inject without rate) error = %v, want ErrInvalidLabSettings", err)
	}

	plainDir := filepath.Join(t.TempDir(), "torrents")
	if err := os.MkdirAll(plainDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	cfg := testConfig()
	cfg.Connections.ListenHost = "127.0.0.1"
	plain, err := session.NewWithClientConfig(cfg, plainDir, func(cc *session.TorrentClientConfig) {
		cc.NoDHT = true
		cc.DisableUTP = true
		cc.DisableIPv6 = true
		cc.NoDefaultPortForwarding = true
	})
	if err != nil {
		t.Fatalf("NewWithClientConfig: %v", err)
	}
	t.Cleanup(func() {
		if err := plain.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if _, err := plain.SetLabSettings(ctx, lab.RuntimeSettings{Mode: lab.ModeBaseline}); !errors.Is(err, session.ErrLabDisabled) {
		t.Fatalf("SetLabSettings without experiment error = %v, want ErrLabDisabled", err)
	}
	if status := plain.LabStatus(); status.Enabled {
		t.Fatalf("LabStatus reports enabled without the experiment: %+v", status)
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

	sess := newLabSession(t, filepath.Join(work, "torrents"), lab.Settings{
		RuntimeSettings: lab.RuntimeSettings{Mode: lab.ModeBaseline},
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
