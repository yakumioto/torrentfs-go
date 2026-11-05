package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/lab"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

func TestGetLabDisabled(t *testing.T) {
	backend := &fakeBackend{}
	srv := newTestServer(t, backend, nil)
	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/lab", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["enabled"] != false {
		t.Fatalf("enabled = %v, want false", got["enabled"])
	}
	if got["settings"] != nil {
		t.Fatalf("settings = %v, want null", got["settings"])
	}
	torrents, ok := got["torrents"].([]any)
	if !ok || len(torrents) != 0 {
		t.Fatalf("torrents = %v, want an empty array", got["torrents"])
	}
}

func TestGetLabEnabled(t *testing.T) {
	backend := &fakeBackend{labStatus: session.LabStatus{
		Enabled: true,
		Settings: session.LabSettings{
			Mode:               lab.ModeInject,
			RateBytesPerSecond: 2 << 20,
			JitterFraction:     0.2,
		},
		Seed:               42,
		AnnounceLogPath:    "/tmp/lab.jsonl",
		PeerSampleInterval: 5 * time.Second,
		Torrents: []session.LabTorrentStatus{
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
	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/lab", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Enabled  bool `json:"enabled"`
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
	if !got.Enabled || got.Settings.Mode != "inject" || got.Settings.RateBytesPerSecond != 2<<20 {
		t.Fatalf("settings = %+v, want enabled inject at 2MiB/s", got.Settings)
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

func TestSetLabSettings(t *testing.T) {
	backend := &fakeBackend{labStatus: session.LabStatus{Enabled: true}}
	srv := newTestServer(t, backend, nil)
	body := `{"mode":"amplify","multiplier":5,"jitter_percent":10}`
	rec := do(t, srv, httptest.NewRequest(http.MethodPut, "/api/v1/lab/settings", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	want := session.LabSettings{Mode: lab.ModeAmplify, Multiplier: 5, JitterFraction: 0.1}
	if backend.setLabSettings != want {
		t.Fatalf("stored settings = %+v, want %+v", backend.setLabSettings, want)
	}
	if backend.labSettingsCalls != 1 {
		t.Fatalf("SetLabSettings calls = %d, want 1", backend.labSettingsCalls)
	}
	var got struct {
		Settings struct {
			Mode          string  `json:"mode"`
			Multiplier    float64 `json:"multiplier"`
			JitterPercent float64 `json:"jitter_percent"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.Settings.Mode != "amplify" || got.Settings.Multiplier != 5 || got.Settings.JitterPercent != 10 {
		t.Fatalf("response settings = %+v", got.Settings)
	}
}

func TestSetLabSettingsDefaults(t *testing.T) {
	backend := &fakeBackend{labStatus: session.LabStatus{Enabled: true}}
	srv := newTestServer(t, backend, nil)
	body := `{"mode":"baseline"}`
	rec := do(t, srv, httptest.NewRequest(http.MethodPut, "/api/v1/lab/settings", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if backend.setLabSettings.JitterFraction != 0.2 {
		t.Fatalf("jitter = %v, want the 20%% default", backend.setLabSettings.JitterFraction)
	}
}

func TestSetLabSettingsRejectsMissingMode(t *testing.T) {
	backend := &fakeBackend{labStatus: session.LabStatus{Enabled: true}}
	srv := newTestServer(t, backend, nil)
	rec := do(t, srv, httptest.NewRequest(http.MethodPut, "/api/v1/lab/settings", strings.NewReader(`{"rate_bytes_per_second":1024}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
	}
	assertLabFailureCode(t, rec, api.LabCodeInvalid, false)
	if backend.labSettingsCalls != 0 {
		t.Fatalf("SetLabSettings called %d times, want 0", backend.labSettingsCalls)
	}
}

func TestSetLabSettingsMapsSessionErrors(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantApplied bool
	}{
		{name: "invalid", err: session.ErrInvalidLabSettings, wantStatus: http.StatusBadRequest, wantCode: api.LabCodeInvalid},
		{name: "disabled", err: session.ErrLabDisabled, wantStatus: http.StatusConflict, wantCode: api.LabCodeDisabled},
		{name: "storage", err: session.ErrLabSettingsStorageUnavailable, wantStatus: http.StatusServiceUnavailable, wantCode: api.LabCodeStorageUnavailable},
		{name: "durability", err: session.ErrLabSettingsDurabilityUnconfirmed, wantStatus: http.StatusServiceUnavailable, wantCode: api.LabCodeDurabilityUnconfirmed, wantApplied: true},
		{name: "wrapped durability", err: errors.Join(session.ErrLabSettingsDurabilityUnconfirmed, errors.New("sync")), wantStatus: http.StatusServiceUnavailable, wantCode: api.LabCodeDurabilityUnconfirmed, wantApplied: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := &fakeBackend{labStatus: session.LabStatus{Enabled: true}, setLabSettingsErr: tc.err}
			srv := newTestServer(t, backend, nil)
			rec := do(t, srv, httptest.NewRequest(http.MethodPut, "/api/v1/lab/settings", strings.NewReader(`{"mode":"baseline"}`)))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			assertLabFailureCode(t, rec, tc.wantCode, tc.wantApplied)
		})
	}
}

func assertLabFailureCode(t *testing.T, rec *httptest.ResponseRecorder, wantCode string, wantApplied bool) {
	t.Helper()
	var got struct {
		Error   string `json:"error"`
		Code    string `json:"code"`
		Applied bool   `json:"applied"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode failure body: %v", err)
	}
	if got.Code != wantCode {
		t.Fatalf("code = %q, want %q", got.Code, wantCode)
	}
	if got.Applied != wantApplied {
		t.Fatalf("applied = %v, want %v", got.Applied, wantApplied)
	}
	if got.Error == "" {
		t.Fatal("failure body has an empty error message")
	}
}
