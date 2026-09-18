package api_test

import (
	"bytes"
	"strings"
	"testing"

	"net/http"
	"net/http/httptest"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/logging"
)

func TestAuthRejectionIsLoggedWithoutBearerToken(t *testing.T) {
	cfg := config.Default()
	dynamicAuth(&cfg)
	var buf bytes.Buffer
	logger, _, err := logging.New(config.Log{Level: "info", Format: "text"}, &buf)
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	srv, err := api.New(cfg, &fakeBackend{}, api.WithLogger(logger))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	secret := "bearer-secret-value"
	req := httptest.NewRequest(http.MethodGet, "/api/v1/torrents", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	if rec := do(t, srv, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	output := buf.String()
	if !strings.Contains(output, "level=WARN") || !strings.Contains(output, "msg=\"http auth rejected\"") {
		t.Fatalf("auth rejection log = %q", output)
	}
	if strings.Contains(output, secret) || strings.Contains(output, "Authorization: Bearer") {
		t.Fatalf("auth rejection log contains credential: %q", output)
	}

}
