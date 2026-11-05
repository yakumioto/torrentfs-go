package main

import (
	"log/slog"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestSyntheticUploadOptionsDisabledByDefault(t *testing.T) {
	for _, key := range []string{
		"TORRENTFS_LAB_FAKE_UPLOAD_RATE",
		"TORRENTFS_LAB_FAKE_UPLOAD_JITTER",
		"TORRENTFS_LAB_FAKE_UPLOAD_CAP",
		"TORRENTFS_LAB_FAKE_UPLOAD_SEED",
	} {
		t.Setenv(key, "")
	}
	options, err := syntheticUploadOptions(testLogger())
	if err != nil {
		t.Fatalf("syntheticUploadOptions: %v", err)
	}
	if len(options) != 0 {
		t.Fatalf("got %d options without lab configuration, want none", len(options))
	}
}

func TestSyntheticUploadOptionsEnabledByRate(t *testing.T) {
	t.Setenv("TORRENTFS_LAB_FAKE_UPLOAD_RATE", "1MiB/s")
	t.Setenv("TORRENTFS_LAB_FAKE_UPLOAD_JITTER", "")
	t.Setenv("TORRENTFS_LAB_FAKE_UPLOAD_CAP", "")
	t.Setenv("TORRENTFS_LAB_FAKE_UPLOAD_SEED", "")
	options, err := syntheticUploadOptions(testLogger())
	if err != nil {
		t.Fatalf("syntheticUploadOptions: %v", err)
	}
	if len(options) != 1 {
		t.Fatalf("got %d options, want the announce director option", len(options))
	}
}

func TestSyntheticUploadOptionsRejectsPartialConfiguration(t *testing.T) {
	t.Setenv("TORRENTFS_LAB_FAKE_UPLOAD_RATE", "")
	t.Setenv("TORRENTFS_LAB_FAKE_UPLOAD_JITTER", "10")
	options, err := syntheticUploadOptions(testLogger())
	if err == nil {
		t.Fatal("syntheticUploadOptions accepted jitter without a rate")
	}
	if options != nil {
		t.Fatalf("options = %v, want nil on configuration error", options)
	}
}
