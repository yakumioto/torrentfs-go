package api_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/stream"
)

// The cap protects the session-wide prefetch budget: an unbounded fan-out of
// signed reads would starve every other player. Overflow fails fast instead of
// queueing, so a client learns to retry rather than hang.
func TestStreamRejectsBeyondTheConcurrencyCap(t *testing.T) {
	b := newStreamTestBackend(t)
	gate := make(chan struct{})
	entered := make(chan struct{}, 16)
	b.beforeOpen = func() {
		entered <- struct{}{}
		<-gate
	}
	srv := newTestServer(t, b, enableStream)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := do(t, srv, httptest.NewRequest(http.MethodGet, b.url(t, ""), nil))
			if rec.Code != http.StatusOK {
				t.Errorf("concurrent stream = %d, want 200; body = %q", rec.Code, rec.Body.String())
			}
		}()
	}
	for range 8 {
		<-entered
	}

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, b.url(t, ""), nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("overflow stream = %d, want 503; body = %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}

	close(gate)
	wg.Wait()
	rec = do(t, srv, httptest.NewRequest(http.MethodGet, b.url(t, ""), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("stream after release = %d, want 200; body = %q", rec.Code, rec.Body.String())
	}
}

// Links carry their own deadline, so a copied or stale .strm value dies without
// any server state to revoke.
func TestStreamRejectsExpiredLinks(t *testing.T) {
	b := newStreamTestBackend(t)
	srv := newTestServer(t, b, enableStream)
	key := bytes.Repeat([]byte{0x25}, stream.KeySize)
	expired, err := stream.NewSigner(key, time.Hour, stream.WithClock(func() time.Time {
		return time.Now().Add(-48 * time.Hour)
	}))
	if err != nil {
		t.Fatal(err)
	}
	url, err := stream.BuildURL("http://example.test", b.hash, expired.Sign(b.hash, b.filePath), b.filePath)
	if err != nil {
		t.Fatal(err)
	}
	rec := do(t, srv, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expired link = %d, want 403; body = %q", rec.Code, rec.Body.String())
	}
	if b.opened.Load() != 0 {
		t.Fatalf("expired link opened %d payload handles", b.opened.Load())
	}
	if rec := do(t, srv, httptest.NewRequest(http.MethodGet, b.url(t, ""), nil)); rec.Code != http.StatusOK {
		t.Fatalf("fresh link = %d, want 200", rec.Code)
	}
}
