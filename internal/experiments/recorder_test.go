package experiments

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestRecorderWritesEveryRecordKind(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "announces.jsonl")
	recorder, err := OpenRecorder(logPath, testLogger())
	if err != nil {
		t.Fatalf("OpenRecorder: %v", err)
	}
	recorder.WriteSettings(Settings{
		Mode:               ModeAmplify,
		Multiplier:         5,
		JitterFraction:     0.2,
		CapBytes:           1 << 30,
		Seed:               7,
		PeerSampleInterval: 2 * time.Second,
	})
	recorder.WriteAnnounce(AnnounceRecord{
		InfoHash:         "aabb",
		Tracker:          "tracker.local",
		Event:            "started",
		Mode:             ModeAmplify,
		RealUploaded:     100,
		RealDelta:        100,
		ReportedUploaded: 500,
		ReportedDelta:    500,
	})
	recorder.WritePeerSample(PeerSampleRecord{
		InfoHash:      "aabb",
		ActivePeers:   2,
		TotalPeers:    3,
		UploadedBytes: 100,
	})
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records := readRecords(t, logPath)
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3", len(records))
	}
	if got := records[0]["type"]; got != recordTypeSettings {
		t.Fatalf("first record type = %v, want %s", got, recordTypeSettings)
	}
	if got := records[1]["type"]; got != recordTypeAnnounce {
		t.Fatalf("second record type = %v, want %s", got, recordTypeAnnounce)
	}
	if got := records[2]["type"]; got != recordTypePeers {
		t.Fatalf("third record type = %v, want %s", got, recordTypePeers)
	}
	if got := records[1]["reported_uploaded"]; got != float64(500) {
		t.Fatalf("announce reported_uploaded = %v, want 500", got)
	}
	if got := records[2]["active_peers"]; got != float64(2) {
		t.Fatalf("peers active_peers = %v, want 2", got)
	}
}

func TestRecorderDropsRecordsAfterClose(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "announces.jsonl")
	recorder, err := OpenRecorder(logPath, testLogger())
	if err != nil {
		t.Fatalf("OpenRecorder: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	recorder.WriteAnnounce(AnnounceRecord{InfoHash: "aabb"})
	if err := recorder.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if records := readRecords(t, logPath); len(records) != 0 {
		t.Fatalf("got %d records after close, want 0", len(records))
	}
}

func TestNilRecorderIsSafe(t *testing.T) {
	var recorder *Recorder
	recorder.WriteSettings(Settings{Mode: ModeBaseline})
	recorder.WriteAnnounce(AnnounceRecord{InfoHash: "aabb"})
	recorder.WritePeerSample(PeerSampleRecord{InfoHash: "aabb"})
	if err := recorder.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

func TestOpenRecorderRejectsMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "announces.jsonl")
	if _, err := OpenRecorder(path, testLogger()); err == nil {
		t.Fatal("OpenRecorder created a file under a missing directory")
	}
}

func TestRecorderConcurrentWritesStayLineDelimited(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "announces.jsonl")
	recorder, err := OpenRecorder(logPath, testLogger())
	if err != nil {
		t.Fatalf("OpenRecorder: %v", err)
	}

	var wait sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for i := 0; i < 50; i++ {
				recorder.WriteAnnounce(AnnounceRecord{InfoHash: "aabb"})
				recorder.WritePeerSample(PeerSampleRecord{InfoHash: "aabb"})
			}
		}()
	}
	wait.Wait()
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if records := readRecords(t, logPath); len(records) != 400 {
		t.Fatalf("got %d records, want 400", len(records))
	}
}

func readRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer func() { _ = file.Close() }()
	var records []map[string]any
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode record %q: %v", scanner.Text(), err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read log: %v", err)
	}
	return records
}
