package api_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/logging"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

type blockingListBackend struct {
	fakeBackend
	started chan struct{}
	release <-chan struct{}
}

func (b *blockingListBackend) ListTorrents() []session.TorrentView {
	close(b.started)
	<-b.release
	return nil
}

func TestShutdownFailureHasNoCalleeErrorLog(t *testing.T) {
	cfg := config.Default()
	var buf bytes.Buffer
	logger, _, err := logging.New(config.Log{Level: "info", Format: "text"}, &buf)
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFn)
	backend := &blockingListBackend{started: make(chan struct{}), release: release}
	srv, err := api.New(cfg, backend, api.WithLogger(logger))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	seedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve API address: %v", err)
	}
	listenAddr := seedListener.Addr().String()
	if err := seedListener.Close(); err != nil {
		t.Fatalf("release reserved API address: %v", err)
	}
	serveDone := make(chan error, 1)
	serveCtx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()
	go func() { serveDone <- srv.Serve(serveCtx, listenAddr) }()

	requestDone := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(time.Second)
		var lastErr error
		for time.Now().Before(deadline) {
			response, err := http.Get("http://" + listenAddr + "/api/v1/torrents")
			if err == nil {
				if response != nil {
					_ = response.Body.Close()
				}
				requestDone <- nil
				return
			}
			lastErr = err
			time.Sleep(time.Millisecond)
		}
		requestDone <- lastErr
	}()
	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("blocking API request did not reach backend")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 50*time.Millisecond)
	shutdownErr := srv.Shutdown(shutdownCtx)
	cancelShutdown()
	if !errors.Is(shutdownErr, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want context deadline", shutdownErr)
	}
	releaseFn()
	cancelServe()
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("blocked API request did not finish")
	}
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("API Serve did not finish")
	}

	output := buf.String()
	if strings.Contains(output, "http api shutdown failed") {
		t.Fatalf("API callee emitted failure log: %q", output)
	}
	if strings.Contains(output, "http api shut down") {
		t.Fatalf("failed API shutdown emitted success log: %q", output)
	}
}

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
