package api_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/api"
)

func startStreamServer(t *testing.T, b *streamTestBackend) (*api.Server, string) {
	t.Helper()
	srv := newTestServer(t, b, enableStream)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, "127.0.0.1:0") }()
	t.Cleanup(func() {
		cancel()
		shutdownCtx, stop := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer stop()
		_ = srv.Shutdown(shutdownCtx)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if address := srv.Addr(); address != "" {
			return srv, strings.Replace(b.url(t, ""), "http://example.test", "http://"+address, 1)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("stream server did not listen")
	return nil, ""
}

func waitStreamHandleClosed(t *testing.T, b *streamTestBackend) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for b.closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if b.closed.Load() != 1 {
		t.Fatalf("closed handles = %d, want 1", b.closed.Load())
	}
}

// Client cancellation must reach both ordinary and multipart payload reads.
func TestStreamClientCancellationReleasesReader(t *testing.T) {
	for _, ranges := range []string{"bytes=0-5", "bytes=0-5,16-22"} {
		t.Run(ranges, func(t *testing.T) {
			b := newStreamTestBackend(t)
			started := make(chan struct{})
			var once sync.Once
			b.read = func(ctx context.Context, _ []byte, _ int64) (int, error) {
				once.Do(func() { close(started) })
				<-ctx.Done()
				return 0, ctx.Err()
			}
			_, target := startStreamServer(t, b)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Range", ranges)
			done := make(chan struct{})
			go func() {
				defer close(done)
				response, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
				if err == nil {
					_, _ = io.Copy(io.Discard, response.Body)
					_ = response.Body.Close()
				}
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("request did not reach content reader")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("cancelled client did not finish")
			}
			waitStreamHandleClosed(t, b)
		})
	}
}

// Shutdown with Background must cancel a peerless read rather than wait for a
// whole movie or the configured content-read timeout to elapse.
func TestStreamShutdownCancelsBlockedReads(t *testing.T) {
	b := newStreamTestBackend(t)
	started := make(chan struct{})
	b.read = func(ctx context.Context, _ []byte, _ int64) (int, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	}
	srv, target := startStreamServer(t, b)
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		response, err := (&http.Client{Timeout: 3 * time.Second}).Get(target)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not reach content reader")
	}
	start := time.Now()
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Shutdown took %s, want less than one second", elapsed)
	}
	select {
	case <-clientDone:
	case <-time.After(time.Second):
		t.Fatal("stream did not finish on shutdown")
	}
	waitStreamHandleClosed(t, b)
	if got := do(t, srv, httptest.NewRequest(http.MethodGet, target, nil)).Code; got != 503 {
		t.Fatalf("stream after shutdown = %d, want 503", got)
	}
}

// Context cancellation does not unblock a network Write. After the caller's
// drain deadline, Close must force that connection out of its blocked write.
func TestStreamShutdownClosesBlockedWriter(t *testing.T) {
	b := newStreamTestBackend(t)
	b.size = 16 << 30
	b.read = func(_ context.Context, p []byte, _ int64) (int, error) {
		clear(p)
		return len(p), nil
	}
	srv, target := startStreamServer(t, b)
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	address, err := net.ResolveTCPAddr("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTCP("tcp", nil, address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n", request.URL.EscapedPath()); err != nil {
		t.Fatal(err)
	}
	stable := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		before := b.reads.Load()
		time.Sleep(50 * time.Millisecond)
		if before > 0 && b.reads.Load() == before {
			stable = true
			break
		}
	}
	if !stable {
		t.Fatal("unread TCP response did not block its writer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := srv.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("forced Shutdown took %s, want less than one second", elapsed)
	}
	waitStreamHandleClosed(t, b)
}
