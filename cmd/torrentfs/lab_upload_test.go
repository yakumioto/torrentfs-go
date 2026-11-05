package main

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func clearLabEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"TORRENTFS_LAB_UPLOAD_MODE",
		"TORRENTFS_LAB_UPLOAD_RATE",
		"TORRENTFS_LAB_UPLOAD_MULTIPLIER",
		"TORRENTFS_LAB_UPLOAD_JITTER",
		"TORRENTFS_LAB_UPLOAD_CAP",
		"TORRENTFS_LAB_UPLOAD_SEED",
		"TORRENTFS_LAB_ANNOUNCE_LOG",
		"TORRENTFS_LAB_PEER_SAMPLE_INTERVAL",
	} {
		t.Setenv(key, "")
	}
}

func TestStartLabExperimentDisabledByDefault(t *testing.T) {
	clearLabEnv(t)
	experiment, err := startLabExperiment(testLogger())
	if err != nil {
		t.Fatalf("startLabExperiment: %v", err)
	}
	if experiment != nil {
		t.Fatalf("experiment = %+v, want nil without lab configuration", experiment)
	}
	// A nil experiment must be safe to use from main's unconditional defers and
	// option assembly.
	experiment.Close()
	if options := experiment.sessionOptions(); len(options) != 0 {
		t.Fatalf("got %d options without an experiment, want none", len(options))
	}
}

func TestStartLabExperimentInjectMode(t *testing.T) {
	clearLabEnv(t)
	t.Setenv("TORRENTFS_LAB_UPLOAD_RATE", "1MiB/s")
	experiment, err := startLabExperiment(testLogger())
	if err != nil {
		t.Fatalf("startLabExperiment: %v", err)
	}
	if experiment == nil {
		t.Fatal("experiment is nil although a rate is configured")
	}
	defer experiment.Close()
	if experiment.settings.Mode != "inject" {
		t.Fatalf("mode = %q, want inject", experiment.settings.Mode)
	}
	if options := experiment.sessionOptions(); len(options) != 1 {
		t.Fatalf("got %d options, want the announce director option", len(options))
	}
}

func TestStartLabExperimentWritesSettingsRecord(t *testing.T) {
	clearLabEnv(t)
	logPath := filepath.Join(t.TempDir(), "announces.jsonl")
	t.Setenv("TORRENTFS_LAB_UPLOAD_MODE", "baseline")
	t.Setenv("TORRENTFS_LAB_ANNOUNCE_LOG", logPath)

	experiment, err := startLabExperiment(testLogger())
	if err != nil {
		t.Fatalf("startLabExperiment: %v", err)
	}
	if experiment == nil {
		t.Fatal("experiment is nil although baseline mode is configured")
	}
	experiment.Close()

	file, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("open announce log: %v", err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		t.Fatalf("announce log is empty: %v", scanner.Err())
	}
	var record map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
		t.Fatalf("decode settings record: %v", err)
	}
	if got := record["type"]; got != "settings" {
		t.Fatalf("record type = %v, want settings", got)
	}
	if got := record["mode"]; got != "baseline" {
		t.Fatalf("record mode = %v, want baseline", got)
	}
}

func TestStartLabExperimentRejectsInvalidConfiguration(t *testing.T) {
	clearLabEnv(t)
	t.Setenv("TORRENTFS_LAB_UPLOAD_JITTER", "10")
	experiment, err := startLabExperiment(testLogger())
	if err == nil {
		t.Fatal("startLabExperiment accepted jitter without a mode or rate")
	}
	if experiment != nil {
		t.Fatalf("experiment = %+v, want nil on configuration error", experiment)
	}
}
