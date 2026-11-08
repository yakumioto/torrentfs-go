package main

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// labConfig returns the configuration of an experiment with the given values.
func labConfig(upload config.UploadExperiment) config.Config {
	cfg := config.Default()
	cfg.Experiments.Upload = upload
	return cfg
}

func TestLabSessionOptionsDisabledByDefault(t *testing.T) {
	options, err := labSessionOptions(labConfig(config.UploadExperiment{}), testLogger())
	if err != nil {
		t.Fatalf("labSessionOptions: %v", err)
	}
	if len(options) != 0 {
		t.Fatalf("got %d options without lab configuration, want none", len(options))
	}
}

func TestLabSessionOptionsInjectMode(t *testing.T) {
	cfg := labConfig(config.UploadExperiment{Rate: config.ByteRate(1 << 20)})
	options, err := labSessionOptions(cfg, testLogger())
	if err != nil {
		t.Fatalf("labSessionOptions: %v", err)
	}
	if len(options) != 1 {
		t.Fatalf("got %d options, want the lab experiment option", len(options))
	}
}

func TestLabSessionOptionsBaselineMode(t *testing.T) {
	cfg := labConfig(config.UploadExperiment{
		Mode:        "baseline",
		AnnounceLog: "/tmp/lab-test.jsonl",
	})
	options, err := labSessionOptions(cfg, testLogger())
	if err != nil {
		t.Fatalf("labSessionOptions: %v", err)
	}
	if len(options) != 1 {
		t.Fatalf("got %d options, want the lab experiment option", len(options))
	}
}

func TestLabSessionOptionsAmplifyMode(t *testing.T) {
	cfg := labConfig(config.UploadExperiment{Multiplier: config.Ratio(3)})
	options, err := labSessionOptions(cfg, testLogger())
	if err != nil {
		t.Fatalf("labSessionOptions: %v", err)
	}
	if len(options) != 1 {
		t.Fatalf("got %d options, want the lab experiment option", len(options))
	}
}

func TestLabSessionOptionsRejectsInvalidConfiguration(t *testing.T) {
	cfg := labConfig(config.UploadExperiment{Jitter: config.Percent(10)})
	options, err := labSessionOptions(cfg, testLogger())
	if err == nil {
		t.Fatal("labSessionOptions accepted jitter without a mode or rate")
	}
	if options != nil {
		t.Fatalf("options = %v, want nil on configuration error", options)
	}
}

func TestLabSessionOptionsRejectsRateWithoutInjectMode(t *testing.T) {
	cfg := labConfig(config.UploadExperiment{Mode: "baseline", Rate: config.ByteRate(1 << 20)})
	if _, err := labSessionOptions(cfg, testLogger()); err == nil {
		t.Fatal("labSessionOptions accepted a rate in baseline mode")
	}
}

// TestLabSessionOptionsNamesTheNewConfigurationKeys covers the contract that
// a rejected experiment names the configuration keys the user now writes.
func TestLabSessionOptionsNamesTheNewConfigurationKeys(t *testing.T) {
	_, err := labSessionOptions(labConfig(config.UploadExperiment{Mode: "turbo"}), testLogger())
	if err == nil {
		t.Fatal("labSessionOptions accepted an unknown mode")
	}
	if !strings.Contains(err.Error(), "experiments.upload.mode") {
		t.Errorf("the error %q does not name experiments.upload.mode", err)
	}
}
