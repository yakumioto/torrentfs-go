package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/experiments"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

func TestGetExperimentAlwaysReturnsData(t *testing.T) {
	backend := &fakeBackend{experimentStatus: session.ExperimentStatus{
		Settings: experiments.RuntimeSettings{Mode: experiments.ModeBaseline, JitterFraction: 0.2},
		Seed:     42,
	}}
	srv := newTestServer(t, backend, nil)
	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/experiments/upload", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, ok := got["enabled"]; ok {
		t.Fatalf("response still reports an enabled field: %v", got)
	}
	settings, ok := got["settings"].(map[string]any)
	if !ok {
		t.Fatalf("settings = %v, want an object", got["settings"])
	}
	if settings["mode"] != "baseline" {
		t.Fatalf("settings.mode = %v, want baseline", settings["mode"])
	}
	torrents, ok := got["torrents"].([]any)
	if !ok || len(torrents) != 0 {
		t.Fatalf("torrents = %v, want an empty array", got["torrents"])
	}
}

func TestGetExperimentReturnsTheLiveComparison(t *testing.T) {
	backend := &fakeBackend{experimentStatus: session.ExperimentStatus{
		Settings: session.ExperimentSettings{
			Mode:               experiments.ModeInject,
			RateBytesPerSecond: 2 << 20,
			JitterFraction:     0.2,
		},
		Seed:               42,
		AnnounceLogPath:    "/tmp/lab.jsonl",
		PeerSampleInterval: 5 * time.Second,
		Torrents: []session.ExperimentTorrentStatus{
			{
				InfoHash:                        "aaaa",
				RealUploadedBytes:               100,
				RealUploadedAtLastAnnounceBytes: 90,
				ReportedUploadedBytes:           5000,
				LastAnnounceAt:                  time.Date(2026, 11, 5, 15, 12, 0, 0, time.UTC),
			},
			{
				InfoHash:                        "bbbb",
				RealUploadedBytes:               50,
				RealUploadedAtLastAnnounceBytes: 40,
				ReportedUploadedBytes:           25,
			},
		},
	}}
	srv := newTestServer(t, backend, nil)
	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/experiments/upload", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Settings struct {
			Mode               string  `json:"mode"`
			RateBytesPerSecond int64   `json:"rate_bytes_per_second"`
			JitterPercent      float64 `json:"jitter_percent"`
		} `json:"settings"`
		Seed                      int64   `json:"seed"`
		AnnounceLog               string  `json:"announce_log"`
		PeerSampleIntervalSeconds float64 `json:"peer_sample_interval_seconds"`
		Totals                    struct {
			RealUploadedBytes               int64 `json:"real_uploaded_bytes"`
			RealUploadedAtLastAnnounceBytes int64 `json:"real_uploaded_at_last_announce_bytes"`
			ReportedUploadedBytes           int64 `json:"reported_uploaded_bytes"`
			OverreportedBytes               int64 `json:"overreported_bytes"`
			PendingRealUploadBytes          int64 `json:"pending_real_upload_bytes"`
		} `json:"totals"`
		Torrents []struct {
			InfoHash                        string  `json:"info_hash"`
			RealUploadedBytes               int64   `json:"real_uploaded_bytes"`
			RealUploadedAtLastAnnounceBytes int64   `json:"real_uploaded_at_last_announce_bytes"`
			ReportedUploadedBytes           int64   `json:"reported_uploaded_bytes"`
			LastAnnounceAt                  *string `json:"last_announce_at"`
		} `json:"torrents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.Settings.Mode != "inject" || got.Settings.RateBytesPerSecond != 2<<20 {
		t.Fatalf("settings = %+v, want inject at 2MiB/s", got.Settings)
	}
	if got.Settings.JitterPercent != 20 {
		t.Fatalf("jitter = %v, want 20", got.Settings.JitterPercent)
	}
	if got.Seed != 42 || got.AnnounceLog != "/tmp/lab.jsonl" || got.PeerSampleIntervalSeconds != 5 {
		t.Fatalf("metadata = seed %d log %q interval %v", got.Seed, got.AnnounceLog, got.PeerSampleIntervalSeconds)
	}
	if got.Totals.RealUploadedBytes != 150 ||
		got.Totals.RealUploadedAtLastAnnounceBytes != 130 ||
		got.Totals.ReportedUploadedBytes != 5025 ||
		got.Totals.OverreportedBytes != 4895 ||
		got.Totals.PendingRealUploadBytes != 20 {
		t.Fatalf("totals = %+v", got.Totals)
	}
	if len(got.Torrents) != 2 || got.Torrents[0].InfoHash != "aaaa" || got.Torrents[1].ReportedUploadedBytes != 25 {
		t.Fatalf("torrents = %+v", got.Torrents)
	}
	if got.Torrents[0].RealUploadedAtLastAnnounceBytes != 90 || got.Torrents[0].LastAnnounceAt == nil {
		t.Fatalf("announce-time fields = %+v", got.Torrents[0])
	}
	if got.Torrents[1].LastAnnounceAt != nil {
		t.Fatalf("zero last announce time should serialize as null, got %v", *got.Torrents[1].LastAnnounceAt)
	}
}
