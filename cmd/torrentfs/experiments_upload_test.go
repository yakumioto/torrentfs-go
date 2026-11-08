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
	settings, err := experimentSettings(config.Default())
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
	}))
	if err != nil {
		t.Fatalf("experimentSettings(inject): %v", err)
	}
	if inject.Mode != experiments.ModeInject || inject.RateBytesPerSecond != 1<<20 {
		t.Fatalf("inject settings = %+v", inject)
	}

	amplify, err := experimentSettings(experimentalConfig(config.UploadExperiment{
		Mode:       "amplify",
		Multiplier: config.Ratio(3),
	}))
	if err != nil {
		t.Fatalf("experimentSettings(amplify): %v", err)
	}
	if amplify.Mode != experiments.ModeAmplify || amplify.Multiplier != 3 {
		t.Fatalf("amplify settings = %+v", amplify)
	}
}

// TestApplyLegacyExperimentRuntimeUsesTheSidecarValues covers the migration
// downgrade: when the sidecar values could not be merged into the
// configuration file, the configuration the session and the page share gains
// them.
func TestApplyLegacyExperimentRuntimeUsesTheSidecarValues(t *testing.T) {
	cfg := config.Default()
	sources := config.Sources{}
	fallback := experiments.RuntimeSettings{
		Mode:               experiments.ModeInject,
		RateBytesPerSecond: 8 << 20,
		JitterFraction:     0.1,
	}
	sources = applyLegacyExperimentRuntime(&cfg, sources, &fallback)
	settings, err := experimentSettings(cfg)
	if err != nil {
		t.Fatalf("experimentSettings: %v", err)
	}
	if settings.Mode != experiments.ModeInject || settings.RateBytesPerSecond != 8<<20 {
		t.Fatalf("settings = %+v, want the legacy fallback", settings)
	}
	if settings.JitterFraction != 0.1 {
		t.Fatalf("jitter = %v, want the legacy 0.1", settings.JitterFraction)
	}
	if got := sources.SourceOf("experiments.upload.mode"); got != config.SourceFile {
		t.Fatalf("the source of experiments.upload.mode = %q, want the closest source file", got)
	}
}

// TestApplyLegacyExperimentRuntimeKeepsEnvironmentValues covers the documented
// precedence during a read-only downgrade: the environment is a configuration
// source, so the retired sidecar must not replace a value it supplies. The
// page reports the same source the session uses.
func TestApplyLegacyExperimentRuntimeKeepsEnvironmentValues(t *testing.T) {
	cfg := config.Default()
	cfg.Experiments.Upload.Jitter = config.Percent(10)
	sources := config.Sources{
		"experiments.upload.jitter": config.SourceEnvironment,
	}
	fallback := experiments.RuntimeSettings{
		Mode:               experiments.ModeInject,
		RateBytesPerSecond: 8 << 20,
		JitterFraction:     0.2,
	}
	sources = applyLegacyExperimentRuntime(&cfg, sources, &fallback)
	settings, err := experimentSettings(cfg)
	if err != nil {
		t.Fatalf("experimentSettings: %v", err)
	}
	if settings.Mode != experiments.ModeInject || settings.RateBytesPerSecond != 8<<20 {
		t.Fatalf("settings = %+v, want the legacy values the environment does not override", settings)
	}
	if settings.JitterFraction != 0.1 {
		t.Fatalf("jitter = %v, want the environment value 0.1", settings.JitterFraction)
	}
	if got := sources.SourceOf("experiments.upload.jitter"); got != config.SourceEnvironment {
		t.Fatalf("the source of experiments.upload.jitter = %q, want environment", got)
	}
	if got := sources.SourceOf("experiments.upload.rate"); got != config.SourceFile {
		t.Fatalf("the source of experiments.upload.rate = %q, want the closest source file", got)
	}
}

// TestExperimentSessionOptionsAlwaysReturnsTheExperiment covers the contract
// that a start always passes the experiment option to the session.
func TestExperimentSessionOptionsAlwaysReturnsTheExperiment(t *testing.T) {
	options, err := experimentSessionOptions(config.Default(), testLogger())
	if err != nil {
		t.Fatalf("experimentSessionOptions: %v", err)
	}
	if len(options) != 1 {
		t.Fatalf("got %d options, want the experiment option", len(options))
	}
}

// TestExperimentSettingsRejectsRateWithoutInjectMode covers the explicit-mode
// contract: a rate alone no longer selects inject.
func TestExperimentSettingsRejectsRateWithoutInjectMode(t *testing.T) {
	_, err := experimentSettings(experimentalConfig(config.UploadExperiment{Rate: config.ByteRate(1 << 20)}))
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
	_, err := experimentSettings(experimentalConfig(config.UploadExperiment{Mode: "turbo"}))
	if err == nil {
		t.Fatal("experimentSettings accepted an unknown mode")
	}
	if !strings.Contains(err.Error(), "experiments.upload.mode") {
		t.Errorf("the error %q does not name experiments.upload.mode", err)
	}
}
