package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

func newHealthServer(t *testing.T, backend api.Backend, opts ...api.Option) *api.Server {
	t.Helper()
	srv, err := api.New(config.Default(), backend, opts...)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return srv
}

func TestHealthzIsAlwaysOK(t *testing.T) {
	srv := newHealthServer(t, &fakeBackend{})
	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
	}
	if body.Status != "ok" {
		t.Fatalf("status field = %q, want ok", body.Status)
	}
}

func TestReadyzReportsReadiness(t *testing.T) {
	ready := newHealthServer(t, &fakeBackend{}, api.WithReadiness(func() error { return nil }))
	rec := do(t, ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ready status = %d, want 200", rec.Code)
	}

	broken := errors.New("fuse mountpoint is not mounted")
	notReady := newHealthServer(t, &fakeBackend{}, api.WithReadiness(func() error { return broken }))
	rec = do(t, notReady, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "fuse mountpoint is not mounted") {
		t.Fatalf("not-ready body = %q, want the reason", rec.Body.String())
	}
}

// TestUnknownPathIsNotServedByTheSPAFallback keeps probes from reading the
// single-page app's 200 as health.
func TestUnknownPathIsNotServedByTheSPAFallback(t *testing.T) {
	srv := newHealthServer(t, &fakeBackend{})
	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /metrics status = %d, want 404", rec.Code)
	}

	// Client-side routes keep falling back to the app shell.
	for _, path := range []string{"/", "/config", "/login", "/torrents/abc"} {
		rec := do(t, srv, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, rec.Code)
		}
	}
}

func TestStatsExposeCachePrefetchAndVersion(t *testing.T) {
	backend := &fakeBackend{runtimeStats: session.RuntimeStatsView{
		CacheHits:                     7,
		CacheMisses:                   3,
		CacheEvictions:                2,
		CacheEvictedBytes:             4096,
		CachePutRejections:            1,
		CachePinReservationRejections: 5,
		PrefetchBufferedBytes:         8192,
		PrefetchActivePieces:          2,
		PrefetchActiveBytes:           16384,
	}}
	srv := newHealthServer(t, backend, api.WithVersion("v1.2.3"))

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Version string `json:"version"`
		Cache   struct {
			Hits                     uint64 `json:"hits"`
			Misses                   uint64 `json:"misses"`
			Evictions                uint64 `json:"evictions"`
			EvictedBytes             int64  `json:"evicted_bytes"`
			PutRejections            uint64 `json:"put_rejections"`
			PinReservationRejections uint64 `json:"pin_reservation_rejections"`
		} `json:"cache"`
		Prefetch struct {
			BufferedBytes int64 `json:"buffered_bytes"`
			ActivePieces  int   `json:"active_pieces"`
			ActiveBytes   int64 `json:"active_bytes"`
		} `json:"prefetch"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
	}
	if body.Version != "v1.2.3" {
		t.Fatalf("version = %q, want v1.2.3", body.Version)
	}
	if body.Cache.Hits != 7 || body.Cache.Misses != 3 || body.Cache.Evictions != 2 {
		t.Fatalf("cache counters = %+v", body.Cache)
	}
	if body.Cache.EvictedBytes != 4096 || body.Cache.PutRejections != 1 || body.Cache.PinReservationRejections != 5 {
		t.Fatalf("cache rejections = %+v", body.Cache)
	}
	if body.Prefetch.BufferedBytes != 8192 || body.Prefetch.ActivePieces != 2 || body.Prefetch.ActiveBytes != 16384 {
		t.Fatalf("prefetch = %+v", body.Prefetch)
	}
}

// TestHealthChecksStayReachableWithoutCredentials pins the deliberate bypass:
// an orchestrator has no token, so the checks answer before the auth middleware
// while the management API keeps rejecting anonymous callers.
func TestHealthChecksStayReachableWithoutCredentials(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, dynamicAuth)
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := do(t, srv, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s without a token = %d, want 200", path, rec.Code)
		}
	}
	if rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous stats = %d, want 401", rec.Code)
	}
}

func TestHealthChecksRejectOtherMethods(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, nil)
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := do(t, srv, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s = %d, want 405", path, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
			t.Fatalf("POST %s Allow = %q, want GET, HEAD", path, got)
		}
	}
}
