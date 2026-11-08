package session_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/configcheck"
	"github.com/yakumioto/torrentfs-go/internal/configctl"
	"github.com/yakumioto/torrentfs-go/internal/experiments"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// TestExperimentApplierHotAppliesRuntimeSettings covers the applier contract:
// it names exactly the runtime keys, and applying them changes the reporting of
// later announces without touching the restart-only settings.
func TestExperimentApplierHotAppliesRuntimeSettings(t *testing.T) {
	sess := newLabSession(t, t.TempDir(), experiments.Settings{
		RuntimeSettings:    experiments.RuntimeSettings{Mode: experiments.ModeBaseline, JitterFraction: 0.2},
		Seed:               7,
		AnnounceLogPath:    "",
		PeerSampleInterval: experiments.DefaultSettings().PeerSampleInterval,
	})

	cfg := config.Default()
	cfg.Experiments.Upload = config.UploadExperiment{
		Mode:   "inject",
		Rate:   config.ByteRate(2 << 20),
		Jitter: config.Percent(10),
	}
	applier := session.NewExperimentApplier(sess)
	wantKeys := []string{
		"experiments.upload.mode",
		"experiments.upload.rate",
		"experiments.upload.multiplier",
		"experiments.upload.jitter",
		"experiments.upload.cap",
	}
	if got := applier.Keys(); len(got) != len(wantKeys) {
		t.Fatalf("Keys() = %v, want %v", got, wantKeys)
	}
	for index, want := range wantKeys {
		if applier.Keys()[index] != want {
			t.Fatalf("Keys() = %v, want %v", applier.Keys(), wantKeys)
		}
	}

	if err := applier.Apply(cfg); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	status := sess.LabStatus()
	if status.Settings.Mode != experiments.ModeInject || status.Settings.RateBytesPerSecond != 2<<20 {
		t.Fatalf("settings = %+v, want inject at 2MiB/s", status.Settings)
	}
	if status.Settings.JitterFraction != 0.10 {
		t.Fatalf("jitter = %v, want 0.10", status.Settings.JitterFraction)
	}
	if status.Seed != 7 {
		t.Fatalf("seed = %d, want the restart-only 7", status.Seed)
	}
}

// TestConfigurationSaveHotAppliesTheExperiment covers the configuration page
// path: saving a new mode and its parameters writes the file and immediately
// changes the running reporter.
func TestConfigurationSaveHotAppliesTheExperiment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	work := t.TempDir()
	configPath := filepath.Join(work, "torrentfs.toml")
	document := `backend = "none"

[http]
listen_addr = "127.0.0.1:0"

[experiments.upload]
mode = "inject"
rate = "1MiB/s"
`
	if err := os.WriteFile(configPath, []byte(document), 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}
	cfg, sources, err := configcheck.Load(configPath)
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}
	settings, err := experiments.SettingsFrom(cfg.Experiments.Upload)
	if err != nil {
		t.Fatalf("SettingsFrom: %v", err)
	}

	torrentsDir := filepath.Join(work, "torrents")
	if err := os.MkdirAll(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	clientCfg := testConfig()
	clientCfg.Connections.ListenHost = "127.0.0.1"
	sess, err := session.NewWithClientConfig(clientCfg, torrentsDir, func(cc *session.TorrentClientConfig) {
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

	controller := configctl.New(configPath, cfg, sources)
	if err := controller.RegisterApplier(session.NewExperimentApplier(sess)); err != nil {
		t.Fatalf("RegisterApplier: %v", err)
	}
	view, err := controller.View()
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	result, err := controller.Save(ctx, configctl.SaveRequest{
		Baseline: view.Baseline,
		Patch: map[string]string{
			"experiments.upload.mode":       "amplify",
			"experiments.upload.rate":       "0B/s",
			"experiments.upload.multiplier": "3",
		},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	for _, key := range []string{"experiments.upload.mode", "experiments.upload.rate", "experiments.upload.multiplier"} {
		found := false
		for _, applied := range result.Applied {
			if applied == key {
				found = true
			}
		}
		if !found {
			t.Fatalf("Applied = %v, want %q", result.Applied, key)
		}
	}
	status := sess.LabStatus()
	if status.Settings.Mode != experiments.ModeAmplify || status.Settings.Multiplier != 3 {
		t.Fatalf("running settings = %+v, want amplify x3", status.Settings)
	}

	// The seed, the announce log, and the peer sample interval are fixed for
	// the lifetime of the process: a save writes them but reports them as
	// restart-required, and the running values do not change.
	seedBefore := status.Seed
	logBefore := status.AnnounceLogPath
	intervalBefore := status.PeerSampleInterval
	view, err = controller.View()
	if err != nil {
		t.Fatalf("View after the first save: %v", err)
	}
	result, err = controller.Save(ctx, configctl.SaveRequest{
		Baseline: view.Baseline,
		Patch: map[string]string{
			"experiments.upload.seed":                 "99",
			"experiments.upload.announce_log":         "/tmp/experiment.jsonl",
			"experiments.upload.peer_sample_interval": "15s",
		},
	})
	if err != nil {
		t.Fatalf("Save restart-only settings: %v", err)
	}
	wantRestart := []string{
		"experiments.upload.seed",
		"experiments.upload.announce_log",
		"experiments.upload.peer_sample_interval",
	}
	for _, key := range wantRestart {
		found := false
		for _, restart := range result.RestartRequired {
			if restart == key {
				found = true
			}
		}
		if !found {
			t.Fatalf("RestartRequired = %v, want %q", result.RestartRequired, key)
		}
	}
	if len(result.Applied) != 0 {
		t.Fatalf("Applied = %v, want no hot-applied key", result.Applied)
	}
	status = sess.LabStatus()
	if status.Seed != seedBefore || status.AnnounceLogPath != logBefore || status.PeerSampleInterval != intervalBefore {
		t.Fatalf("restart-only settings changed at runtime: before %d/%q/%v, after %d/%q/%v",
			seedBefore, logBefore, intervalBefore, status.Seed, status.AnnounceLogPath, status.PeerSampleInterval)
	}
}
