package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

// A v2-only magnet is refused on purpose. The response must name the missing
// support instead of implying the caller forgot an info hash.
func TestAddV2OnlyMagnetExplainsUnsupported(t *testing.T) {
	backend := &fakeBackend{addErr: session.ErrV2Only}
	srv := newTestServer(t, backend, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/torrents",
		strings.NewReader(`{"magnet_uri":"magnet:?xt=urn:btmh:1220`+strings.Repeat("ab", 32)+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := do(t, srv, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "v2-only torrents are not supported (no v1 info hash)") {
		t.Fatalf("body = %q, want the v2-only explanation", rec.Body.String())
	}
}
