package api_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/logging"
)

// Application logs must not retain a capability, including when an underlying
// reader's diagnostic happens to contain the complete signed URL.
func TestStreamLogsRedactCapabilities(t *testing.T) {
	b := newStreamTestBackend(t)
	cfg := config.Default()
	enableStream(&cfg)
	var output bytes.Buffer
	logger, _, err := logging.New(config.Log{Level: "info", Format: "text"}, &output)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := api.New(cfg, b, api.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	link := b.url(t, "")
	signature := b.signer.Sign(b.hash, b.filePath)
	badSignature := strings.Repeat("A", len(signature))
	badURL := strings.Replace(link, signature, badSignature, 1)
	if got := do(t, srv, httptest.NewRequest(http.MethodGet, badURL, nil)).Code; got != 403 {
		t.Fatalf("bad signature status = %d, want 403", got)
	}
	b.read = func(context.Context, []byte, int64) (int, error) {
		return 0, errors.New("source failure for " + link)
	}
	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler {
				t.Fatalf("read failure panic = %v, want ErrAbortHandler", recovered)
			}
		}()
		do(t, srv, httptest.NewRequest(http.MethodGet, link, nil))
	}()
	logged := output.String()
	if !strings.Contains(logged, "http stream signature rejected") || !strings.Contains(logged, "http stream interrupted") {
		t.Fatalf("missing stream diagnostics: %q", logged)
	}
	for _, secret := range []string{link, badURL, signature, badSignature, b.filePath} {
		if strings.Contains(logged, secret) {
			t.Fatalf("stream log leaked a capability or file path: %q", logged)
		}
	}
}
