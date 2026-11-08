package experiments

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPeerSamplerSamplesEveryTorrent(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "announces.jsonl")
	recorder, err := OpenRecorder(logPath, testLogger())
	if err != nil {
		t.Fatalf("OpenRecorder: %v", err)
	}
	sampler := NewPeerSampler(recorder, time.Minute, func() []PeerSnapshot {
		return []PeerSnapshot{
			{InfoHash: "aaaa", UploadedBytes: 10, DownloadedBytes: 20, TotalPeers: 3, PendingPeers: 1, ActivePeers: 2, ConnectedSeeders: 1},
			{InfoHash: "bbbb", UploadedBytes: 30, DownloadedBytes: 40, TotalPeers: 5, ActivePeers: 4},
		}
	})
	sampler.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	sampler.sample()
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records := readRecords(t, logPath)
	if len(records) != 2 {
		t.Fatalf("got %d peer samples, want 2", len(records))
	}
	if got := records[0]["info_hash"]; got != "aaaa" {
		t.Fatalf("first info hash = %v, want aaaa", got)
	}
	if got := records[1]["uploaded_bytes"]; got != float64(30) {
		t.Fatalf("second uploaded_bytes = %v, want 30", got)
	}
}

func TestPeerSamplerRunRecordsImmediatelyAndStops(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "announces.jsonl")
	recorder, err := OpenRecorder(logPath, testLogger())
	if err != nil {
		t.Fatalf("OpenRecorder: %v", err)
	}
	sampler := NewPeerSampler(recorder, time.Hour, func() []PeerSnapshot {
		return []PeerSnapshot{{InfoHash: "aaaa"}}
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sampler.Run(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sampler did not stop after context cancellation")
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if records := readRecords(t, logPath); len(records) != 1 {
		t.Fatalf("got %d peer samples, want the immediate sample", len(records))
	}
}

func TestPeerSamplerRunIsInertWithoutConfiguration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var nilSampler *PeerSampler
	nilSampler.Run(ctx)
	NewPeerSampler(nil, time.Second, nil).Run(ctx)
}

func TestNewPeerSamplerFallsBackToDefaultInterval(t *testing.T) {
	sampler := NewPeerSampler(nil, 0, nil)
	if sampler.interval != defaultPeerSampleInterval {
		t.Fatalf("interval = %v, want %v", sampler.interval, defaultPeerSampleInterval)
	}
}
