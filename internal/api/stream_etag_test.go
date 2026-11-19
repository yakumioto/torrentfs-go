package api_test

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/stream"
)

// streamETag mirrors the production derivation: the validator must depend on
// the requested file, never on the capability that authorized the request.
func streamETag(hash metainfo.Hash, path string) string {
	sum := sha256.Sum256([]byte(hash.HexString() + "\x00" + path))
	return fmt.Sprintf("%q", fmt.Sprintf("%x", sum[:8]))
}

func TestStreamETagIgnoresTheSignature(t *testing.T) {
	backend := newStreamTestBackend(t)
	srv := newTestServer(t, backend, func(cfg *config.Config) {
		enableStream(cfg)
	})

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, backend.url(t, ""), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	signature := backend.signer.Sign(backend.hash, backend.filePath)
	if etag == "" {
		t.Fatal("missing ETag")
	}
	if strings.Contains(etag, signature) {
		t.Fatalf("ETag %q leaks the request signature", etag)
	}
	if etag != streamETag(backend.hash, backend.filePath) {
		t.Fatalf("ETag = %q, want the path-derived validator", etag)
	}

	// A second request for the same file keeps the validator stable, so cached
	// conditional requests keep working across links.
	again := do(t, srv, httptest.NewRequest(http.MethodGet, backend.url(t, ""), nil))
	if got := again.Header().Get("ETag"); got != etag {
		t.Fatalf("ETag changed between requests: %q then %q", etag, got)
	}

	// A different file of the same torrent must not share the validator.
	backend.filePath = "Season 1/第二集.mkv"
	other := do(t, srv, httptest.NewRequest(http.MethodGet, backend.url(t, ""), nil))
	if got := other.Header().Get("ETag"); got == etag || got == "" {
		t.Fatalf("ETag for a second file = %q, want a distinct validator", got)
	}
}

// A rotated signature on the same file still satisfies a conditional request,
// which is the whole point of decoupling the validator from the capability.
func TestStreamETagSurvivesSignatureRotation(t *testing.T) {
	backend := newStreamTestBackend(t)
	srv := newTestServer(t, backend, func(cfg *config.Config) {
		enableStream(cfg)
	})

	first := do(t, srv, httptest.NewRequest(http.MethodGet, backend.url(t, ""), nil))
	etag := first.Header().Get("ETag")

	rotatedSigner, err := stream.NewSigner(make([]byte, stream.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	backend.signer = rotatedSigner
	req := httptest.NewRequest(http.MethodGet, backend.url(t, ""), nil)
	req.Header.Set("If-None-Match", etag)
	rec := do(t, srv, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("conditional request after rotation = %d, want 304", rec.Code)
	}
}
