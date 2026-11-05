package main

import (
	"log/slog"
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

func TestLabSessionOptionsDisabledByDefault(t *testing.T) {
	clearLabEnv(t)
	options, err := labSessionOptions(testLogger())
	if err != nil {
		t.Fatalf("labSessionOptions: %v", err)
	}
	if len(options) != 0 {
		t.Fatalf("got %d options without lab configuration, want none", len(options))
	}
}

func TestLabSessionOptionsInjectMode(t *testing.T) {
	clearLabEnv(t)
	t.Setenv("TORRENTFS_LAB_UPLOAD_RATE", "1MiB/s")
	options, err := labSessionOptions(testLogger())
	if err != nil {
		t.Fatalf("labSessionOptions: %v", err)
	}
	if len(options) != 1 {
		t.Fatalf("got %d options, want the lab experiment option", len(options))
	}
}

func TestLabSessionOptionsBaselineMode(t *testing.T) {
	clearLabEnv(t)
	t.Setenv("TORRENTFS_LAB_UPLOAD_MODE", "baseline")
	t.Setenv("TORRENTFS_LAB_ANNOUNCE_LOG", "/tmp/lab-test.jsonl")
	options, err := labSessionOptions(testLogger())
	if err != nil {
		t.Fatalf("labSessionOptions: %v", err)
	}
	if len(options) != 1 {
		t.Fatalf("got %d options, want the lab experiment option", len(options))
	}
}

func TestLabSessionOptionsRejectsInvalidConfiguration(t *testing.T) {
	clearLabEnv(t)
	t.Setenv("TORRENTFS_LAB_UPLOAD_JITTER", "10")
	options, err := labSessionOptions(testLogger())
	if err == nil {
		t.Fatal("labSessionOptions accepted jitter without a mode or rate")
	}
	if options != nil {
		t.Fatalf("options = %v, want nil on configuration error", options)
	}
}
