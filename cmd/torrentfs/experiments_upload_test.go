package main

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/experiments"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// experimentalConfig returns the default configuration with the upload
// experiment values replaced.
func experimentalConfig(upload config.UploadExperiment) config.Config {
	cfg := config.Default()
	cfg.Experiments.Upload = upload
	return cfg
}

// TestExperimentSettingsDefaultsToBaseline covers the contract that a start
// always runs the experiment: no configuration means the baseline arm, not an
// absent experiment.
func TestExperimentSettingsDefaultsToBaseline(t *testing.T) {
	settings, err := experimentSettings(config.Default(), nil)
	if err != nil {
		t.Fatalf("experimentSettings: %v", err)
	}
	if settings.Mode != experiments.ModeBaseline {
		t.Fatalf("mode = %q, want baseline", settings.Mode)
	}
}

// TestExperimentSettingsUsesTheConfiguredValues covers the configured arms.
func TestExperimentSettingsUsesTheConfiguredValues(t *testing.T) {
	inject, err := experimentSettings(experimentalConfig(config.UploadExperiment{
		Mode: "inject",
		Rate: config.ByteRate(1 << 20),
	}), nil)
	if err != nil {
		t.Fatalf("experimentSettings(inject): %v", err)
	}
	if inject.Mode != experiments.ModeInject || inject.RateBytesPerSecond != 1<<20 {
		t.Fatalf("inject settings = %+v", inject)
	}

	amplify, err := experimentSettings(experimentalConfig(config.UploadExperiment{
		Mode:       "amplify",
		Multiplier: config.Ratio(3),
	}), nil)
	if err != nil {
		t.Fatalf("experimentSettings(amplify): %v", err)
	}
	if amplify.Mode != experiments.ModeAmplify || amplify.Multiplier != 3 {
		t.Fatalf("amplify settings = %+v", amplify)
	}
}

// TestExperimentSettingsUsesTheLegacyFallback covers the migration downgrade:
// when the sidecar values could not be merged into the configuration file the
// running process still uses them.
func TestExperimentSettingsUsesTheLegacyFallback(t *testing.T) {
	fallback := experiments.RuntimeSettings{
		Mode:               experiments.ModeInject,
		RateBytesPerSecond: 8 << 20,
		JitterFraction:     0.1,
	}
	settings, err := experimentSettings(config.Default(), &fallback)
	if err != nil {
		t.Fatalf("experimentSettings: %v", err)
	}
	if settings.Mode != experiments.ModeInject || settings.RateBytesPerSecond != 8<<20 {
		t.Fatalf("settings = %+v, want the legacy fallback", settings)
	}
	if settings.JitterFraction != 0.1 {
		t.Fatalf("jitter = %v, want the legacy 0.1", settings.JitterFraction)
	}
}

// TestLabSessionOptionsAlwaysReturnsTheExperiment covers the contract that a
// start always passes the experiment option to the session.
func TestLabSessionOptionsAlwaysReturnsTheExperiment(t *testing.T) {
	options, err := labSessionOptions(config.Default(), nil, testLogger())
	if err != nil {
		t.Fatalf("labSessionOptions: %v", err)
	}
	if len(options) != 1 {
		t.Fatalf("got %d options, want the experiment option", len(options))
	}
}

// TestExperimentSettingsRejectsRateWithoutInjectMode covers the explicit-mode
// contract: a rate alone no longer selects inject.
func TestExperimentSettingsRejectsRateWithoutInjectMode(t *testing.T) {
	_, err := experimentSettings(experimentalConfig(config.UploadExperiment{Rate: config.ByteRate(1 << 20)}), nil)
	if err == nil {
		t.Fatal("experimentSettings accepted a rate in the baseline mode")
	}
	for _, want := range []string{"experiments.upload.rate", `mode = "inject"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not contain %q", err, want)
		}
	}
}

// TestExperimentSettingsNamesTheNewConfigurationKeys covers the contract that a
// rejected experiment names the configuration keys the user now writes.
func TestExperimentSettingsNamesTheNewConfigurationKeys(t *testing.T) {
	_, err := experimentSettings(experimentalConfig(config.UploadExperiment{Mode: "turbo"}), nil)
	if err == nil {
		t.Fatal("experimentSettings accepted an unknown mode")
	}
	if !strings.Contains(err.Error(), "experiments.upload.mode") {
		t.Errorf("the error %q does not name experiments.upload.mode", err)
	}
}
