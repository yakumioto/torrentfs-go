package main

import (
	"log/slog"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// labConfig returns the configuration of an experiment with the given values.
func labConfig(lab config.Lab) config.Config {
	cfg := config.Default()
	cfg.Lab = lab
	return cfg
}

func TestLabSessionOptionsDisabledByDefault(t *testing.T) {
	options, err := labSessionOptions(labConfig(config.Lab{}), testLogger())
	if err != nil {
		t.Fatalf("labSessionOptions: %v", err)
	}
	if len(options) != 0 {
		t.Fatalf("got %d options without lab configuration, want none", len(options))
	}
}

func TestLabSessionOptionsInjectMode(t *testing.T) {
	cfg := labConfig(config.Lab{UploadRate: config.ByteRate(1 << 20)})
	options, err := labSessionOptions(cfg, testLogger())
	if err != nil {
		t.Fatalf("labSessionOptions: %v", err)
	}
	if len(options) != 1 {
		t.Fatalf("got %d options, want the lab experiment option", len(options))
	}
}

func TestLabSessionOptionsBaselineMode(t *testing.T) {
	cfg := labConfig(config.Lab{
		UploadMode:  "baseline",
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
	cfg := labConfig(config.Lab{UploadMultiplier: config.Ratio(3)})
	options, err := labSessionOptions(cfg, testLogger())
	if err != nil {
		t.Fatalf("labSessionOptions: %v", err)
	}
	if len(options) != 1 {
		t.Fatalf("got %d options, want the lab experiment option", len(options))
	}
}

func TestLabSessionOptionsRejectsInvalidConfiguration(t *testing.T) {
	cfg := labConfig(config.Lab{UploadJitter: config.Percent(10)})
	options, err := labSessionOptions(cfg, testLogger())
	if err == nil {
		t.Fatal("labSessionOptions accepted jitter without a mode or rate")
	}
	if options != nil {
		t.Fatalf("options = %v, want nil on configuration error", options)
	}
}

func TestLabSessionOptionsRejectsRateWithoutInjectMode(t *testing.T) {
	cfg := labConfig(config.Lab{UploadMode: "baseline", UploadRate: config.ByteRate(1 << 20)})
	if _, err := labSessionOptions(cfg, testLogger()); err == nil {
		t.Fatal("labSessionOptions accepted a rate in baseline mode")
	}
}
