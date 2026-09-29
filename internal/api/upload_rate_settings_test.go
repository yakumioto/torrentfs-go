package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

const uploadRateSettingsPath = "/api/v1/settings/upload-rate"

func uploadRateRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

type uploadRateSettingsResponse struct {
	RateLimitBytesPerSecond int64 `json:"rate_limit_bytes_per_second"`
	Schedule                *struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"schedule"`
}

type uploadRateSettingsErrorResponse struct {
	Error   string `json:"error"`
	Code    string `json:"code"`
	Applied *bool  `json:"applied"`
}

func TestGetUploadRateSettings(t *testing.T) {
	backend := &fakeBackend{uploadRateSettings: session.UploadRateSettings{
		RateLimitBytesPerSecond: 1048576,
		Schedule:                &session.UploadRateSchedule{Start: "08:00", End: "22:00"},
	}}
	srv := newTestServer(t, backend, nil)

	rec := do(t, srv, uploadRateRequest(t, http.MethodGet, uploadRateSettingsPath, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body uploadRateSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.RateLimitBytesPerSecond != 1048576 {
		t.Fatalf("rate = %d, want 1048576", body.RateLimitBytesPerSecond)
	}
	if body.Schedule == nil || body.Schedule.Start != "08:00" || body.Schedule.End != "22:00" {
		t.Fatalf("schedule = %+v, want 08:00-22:00", body.Schedule)
	}
	// The storage schema's version field is internal and must stay off the wire.
	if strings.Contains(rec.Body.String(), "version") {
		t.Fatalf("GET response leaks the storage schema: %s", rec.Body.String())
	}
}

func TestGetUploadRateSettingsWithoutScheduleSendsNull(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, nil)

	rec := do(t, srv, uploadRateRequest(t, http.MethodGet, uploadRateSettingsPath, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := string(raw["schedule"]); got != "null" {
		t.Fatalf("schedule = %s, want null", got)
	}
	if got := string(raw["rate_limit_bytes_per_second"]); got != "0" {
		t.Fatalf("rate = %s, want 0", got)
	}
}

func TestPutUploadRateSettings(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantRate     int64
		wantSchedule bool
		wantStart    string
		wantEnd      string
	}{
		{
			name:     "disabled",
			body:     `{"rate_limit_bytes_per_second":0,"schedule":null}`,
			wantRate: 0,
		},
		{
			name:     "all day",
			body:     `{"rate_limit_bytes_per_second":65536}`,
			wantRate: 65536,
		},
		{
			name:         "scheduled",
			body:         `{"rate_limit_bytes_per_second":262144,"schedule":{"start":"06:30","end":"18:30"}}`,
			wantRate:     262144,
			wantSchedule: true,
			wantStart:    "06:30",
			wantEnd:      "18:30",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &fakeBackend{}
			srv := newTestServer(t, backend, nil)

			rec := do(t, srv, uploadRateRequest(t, http.MethodPut, uploadRateSettingsPath, tt.body))
			if rec.Code != http.StatusOK {
				t.Fatalf("PUT status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			var body uploadRateSettingsResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.RateLimitBytesPerSecond != tt.wantRate {
				t.Fatalf("response rate = %d, want %d", body.RateLimitBytesPerSecond, tt.wantRate)
			}
			if (body.Schedule != nil) != tt.wantSchedule {
				t.Fatalf("response schedule = %+v, want present=%t", body.Schedule, tt.wantSchedule)
			}
			if tt.wantSchedule && (body.Schedule.Start != tt.wantStart || body.Schedule.End != tt.wantEnd) {
				t.Fatalf("response schedule = %+v, want %s-%s", body.Schedule, tt.wantStart, tt.wantEnd)
			}

			if backend.uploadRateSettingsCalls != 1 {
				t.Fatalf("backend setter calls = %d, want 1", backend.uploadRateSettingsCalls)
			}
			applied := backend.setUploadRateSettings
			if applied.RateLimitBytesPerSecond != tt.wantRate {
				t.Fatalf("applied rate = %d, want %d", applied.RateLimitBytesPerSecond, tt.wantRate)
			}
			if (applied.Schedule != nil) != tt.wantSchedule {
				t.Fatalf("applied schedule = %+v, want present=%t", applied.Schedule, tt.wantSchedule)
			}
		})
	}
}

func TestPutUploadRateSettingsRejectsInvalidBody(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed json", body: `{"rate_limit_bytes_per_second":`},
		{name: "wrong type", body: `{"rate_limit_bytes_per_second":"fast"}`},
		{name: "missing rate", body: `{}`},
		{name: "null rate", body: `{"rate_limit_bytes_per_second":null}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &fakeBackend{}
			srv := newTestServer(t, backend, nil)

			rec := do(t, srv, uploadRateRequest(t, http.MethodPut, uploadRateSettingsPath, tt.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			var body uploadRateSettingsErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.Code != api.UploadRateSettingsCodeInvalid {
				t.Fatalf("code = %q, want %q", body.Code, api.UploadRateSettingsCodeInvalid)
			}
			if body.Applied == nil || *body.Applied {
				t.Fatalf("applied = %v, want false", body.Applied)
			}
			if backend.uploadRateSettingsCalls != 0 {
				t.Fatalf("backend setter calls = %d, want 0", backend.uploadRateSettingsCalls)
			}
		})
	}
}

// TestPutUploadRateSettingsMapsSessionErrors pins the contract a client branches
// on: an invalid value is a 400, a value the session refused is a 503 that
// reports whether the running limit already changed.
func TestPutUploadRateSettingsMapsSessionErrors(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantApplied bool
	}{
		{
			name:        "invalid settings",
			err:         errors.Join(session.ErrInvalidUploadRateSettings, errors.New("rate_limit_bytes_per_second must not be negative")),
			wantStatus:  http.StatusBadRequest,
			wantCode:    api.UploadRateSettingsCodeInvalid,
			wantApplied: false,
		},
		{
			name:        "storage unavailable",
			err:         session.ErrUploadRateSettingsStorageUnavailable,
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    api.UploadRateSettingsCodeStorageUnavailable,
			wantApplied: false,
		},
		{
			name:        "durability unconfirmed",
			err:         session.ErrUploadRateSettingsDurabilityUnconfirmed,
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    api.UploadRateSettingsCodeDurabilityUnconfirmed,
			wantApplied: true,
		},
		{
			name:        "session closed",
			err:         filesystem.ErrClosed,
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    api.UploadRateSettingsCodeStorageUnavailable,
			wantApplied: false,
		},
		{
			name:        "request cancelled",
			err:         context.Canceled,
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    api.UploadRateSettingsCodeStorageUnavailable,
			wantApplied: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &fakeBackend{setUploadRateSettingsErr: tt.err}
			srv := newTestServer(t, backend, nil)

			rec := do(t, srv, uploadRateRequest(t, http.MethodPut, uploadRateSettingsPath, `{"rate_limit_bytes_per_second":1024}`))
			if rec.Code != tt.wantStatus {
				t.Fatalf("PUT status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			var body uploadRateSettingsErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.Code != tt.wantCode {
				t.Fatalf("code = %q, want %q", body.Code, tt.wantCode)
			}
			if body.Applied == nil || *body.Applied != tt.wantApplied {
				t.Fatalf("applied = %v, want %t", body.Applied, tt.wantApplied)
			}
			if strings.Contains(body.Error, "/") {
				t.Fatalf("error message leaks a host path: %q", body.Error)
			}
		})
	}
}

func TestPutUploadRateSettingsRejectsOversizedBody(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, nil)
	body := `{"rate_limit_bytes_per_second":` + strings.Repeat("1", 8<<10) + `}`

	rec := do(t, srv, uploadRateRequest(t, http.MethodPut, uploadRateSettingsPath, body))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("PUT status = %d, want 413: %s", rec.Code, rec.Body.String())
	}
}

// TestUploadRateSettingsRequiresAuthentication keeps the new routes behind the
// existing bearer middleware instead of adding an unauthenticated exception.
func TestUploadRateSettingsRequiresAuthentication(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, func(cfg *config.Config) {
		cfg.HTTP.Auth.Enabled = true
		cfg.HTTP.Auth.Username = "alice"
		cfg.HTTP.Auth.PasswordHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
	})

	for _, method := range []string{http.MethodGet, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			body := ""
			if method == http.MethodPut {
				body = `{"rate_limit_bytes_per_second":1024}`
			}
			rec := do(t, srv, uploadRateRequest(t, method, uploadRateSettingsPath, body))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s status = %d, want 401: %s", method, rec.Code, rec.Body.String())
			}
		})
	}
}
