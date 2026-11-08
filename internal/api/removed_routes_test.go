package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRetiredUploadRateSettingsRoutesAreGone covers the breaking part of the
// schema migration: the upload limit is configured through the configuration
// API, and the retired settings endpoint no longer answers.
func TestRetiredUploadRateSettingsRoutesAreGone(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, nil)

	for _, method := range []string{http.MethodGet, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			body := ""
			if method == http.MethodPut {
				body = `{"rate_limit_bytes_per_second":1024}`
			}
			req := httptest.NewRequest(method, "/api/v1/settings/upload-rate", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := do(t, srv, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s status = %d, want 404: %s", method, rec.Code, rec.Body.String())
			}
		})
	}
}
