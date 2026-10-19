package api_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/stream"
)

type streamTestBackend struct {
	fakeBackend
	signer   *stream.Signer
	hash     metainfo.Hash
	filePath string
	data     []byte
	size     int64
	openErr  error
	read     func(context.Context, []byte, int64) (int, error)
	opened   atomic.Int32
	reads    atomic.Int32
	closed   atomic.Int32
}

func newStreamTestBackend(t *testing.T) *streamTestBackend {
	t.Helper()
	signer, err := stream.NewSigner(bytes.Repeat([]byte{0x25}, stream.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	return &streamTestBackend{
		signer: signer, hash: metainfo.Hash{0x11},
		filePath: "Season 1/电影 %#?+.mkv", data: data, size: int64(len(data)),
	}
}

func (b *streamTestBackend) OpenStream(loc stream.Location) (stream.File, error) {
	if !b.signer.Verify(loc.Hash, loc.Path, loc.Signature) {
		return stream.File{}, stream.ErrInvalidSignature
	}
	if b.openErr != nil {
		return stream.File{}, b.openErr
	}
	if loc.Hash != b.hash || loc.Path != b.filePath {
		return stream.File{}, filesystem.ErrNotFound
	}
	b.opened.Add(1)
	return stream.File{
		Reader: &streamTestReader{backend: b}, Size: b.size,
		ModifiedAt: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC),
	}, nil
}

func (b *streamTestBackend) url(t *testing.T, filePath string) string {
	t.Helper()
	if filePath == "" {
		filePath = b.filePath
	}
	url, err := stream.BuildURL("http://example.test", b.hash, b.signer.Sign(b.hash, filePath), filePath)
	if err != nil {
		t.Fatal(err)
	}
	return url
}

func enableStream(cfg *config.Config) { cfg.STRM.BaseURL = "http://example.test" }

type streamTestReader struct{ backend *streamTestBackend }

func (r *streamTestReader) ReadAt([]byte, int64) (int, error) {
	return 0, errors.New("stream used ReaderAt without request context")
}

func (r *streamTestReader) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	b := r.backend
	b.reads.Add(1)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if b.read != nil {
		return b.read(ctx, p, off)
	}
	return bytes.NewReader(b.data).ReadAt(p, off)
}

func (r *streamTestReader) Close() error {
	r.backend.closed.Add(1)
	return nil
}

// Range and conditional requests use standard HTTP semantics without requiring
// the management API's session token or sniffing the first bytes of a video.
func TestStreamHTTPRangesAndConditions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		method  string
		headers map[string]string
		status  int
		body    string
		rangeOf string
		noRead  bool
	}{
		{name: "whole", status: 200, body: "0123456789abcdefghijklmnopqrstuvwxyz"},
		{name: "range", headers: map[string]string{"Range": "bytes=10-15"}, status: 206, body: "abcdef", rangeOf: "bytes 10-15/36"},
		{name: "suffix", headers: map[string]string{"Range": "bytes=-3"}, status: 206, body: "xyz", rangeOf: "bytes 33-35/36"},
		{name: "open range", headers: map[string]string{"Range": "bytes=33-"}, status: 206, body: "xyz", rangeOf: "bytes 33-35/36"},
		{name: "head", method: http.MethodHead, status: 200, noRead: true},
		{name: "head range", method: http.MethodHead, headers: map[string]string{"Range": "bytes=10-15"}, status: 206, rangeOf: "bytes 10-15/36", noRead: true},
		{name: "head multipart", method: http.MethodHead, headers: map[string]string{"Range": "bytes=1-2,10-11"}, status: 206, noRead: true},
		{name: "not modified", headers: map[string]string{"If-None-Match": "{etag}"}, status: 304, noRead: true},
		{name: "if modified since", headers: map[string]string{"If-Modified-Since": "Thu, 02 Jan 2025 03:04:05 GMT"}, status: 304, noRead: true},
		{name: "if range matches", headers: map[string]string{"Range": "bytes=1-2", "If-Range": "{etag}"}, status: 206, body: "12", rangeOf: "bytes 1-2/36"},
		{name: "if range differs", headers: map[string]string{"Range": "bytes=1-2", "If-Range": `"old"`}, status: 200, body: "0123456789abcdefghijklmnopqrstuvwxyz"},
		{name: "if match differs", headers: map[string]string{"If-Match": `"old"`}, status: 412, noRead: true},
		{name: "out of range", headers: map[string]string{"Range": "bytes=100-"}, status: 416, rangeOf: "bytes */36", noRead: true},
		{name: "invalid range", headers: map[string]string{"Range": "bytes=bad"}, status: 416, noRead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newStreamTestBackend(t)
			srv := newTestServer(t, b, func(cfg *config.Config) {
				enableStream(cfg)
				dynamicAuth(cfg)
			})
			method := tc.method
			if method == "" {
				method = http.MethodGet
			}
			req := httptest.NewRequest(method, b.url(t, ""), nil)
			etag := `"` + b.hash.HexString() + "-" + b.signer.Sign(b.hash, b.filePath) + `"`
			for name, value := range tc.headers {
				req.Header.Set(name, strings.ReplaceAll(value, "{etag}", etag))
			}
			rec := do(t, srv, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %q", rec.Code, tc.status, rec.Body.String())
			}
			if tc.status < 400 && rec.Body.String() != tc.body {
				t.Fatalf("body = %q, want %q", rec.Body.String(), tc.body)
			}
			if got := rec.Header().Get("Content-Range"); got != tc.rangeOf {
				t.Fatalf("Content-Range = %q, want %q", got, tc.rangeOf)
			}
			if rec.Code == 200 || rec.Code == 206 {
				if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
					t.Fatalf("Accept-Ranges = %q, want bytes", got)
				}
				if rec.Header().Get("ETag") != etag || rec.Header().Get("Content-Length") == "" {
					t.Fatalf("missing ETag or Content-Length: %v", rec.Header())
				}
			}
			if tc.noRead && b.reads.Load() != 0 {
				t.Fatalf("metadata request read payload %d times", b.reads.Load())
			}
			if b.opened.Load() != 1 || b.closed.Load() != 1 {
				t.Fatalf("opened/closed = %d/%d, want 1/1", b.opened.Load(), b.closed.Load())
			}
			if got := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/torrents", nil)).Code; got != 401 {
				t.Fatalf("management API without token = %d, want 401", got)
			}
		})
	}
}

func TestStreamMultipartRange(t *testing.T) {
	b := newStreamTestBackend(t)
	srv := newTestServer(t, b, enableStream)
	req := httptest.NewRequest(http.MethodGet, b.url(t, ""), nil)
	req.Header.Set("Range", "bytes=1-2,10-12")
	rec := do(t, srv, req)
	if rec.Code != 206 {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	kind, params, err := mime.ParseMediaType(rec.Header().Get("Content-Type"))
	if err != nil || kind != "multipart/byteranges" {
		t.Fatalf("Content-Type = %q: %v", kind, err)
	}
	parts := multipart.NewReader(rec.Body, params["boundary"])
	for i, want := range []string{"12", "abc"} {
		part, err := parts.NextPart()
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		got, err := io.ReadAll(part)
		if err != nil || string(got) != want || part.Header.Get("Content-Type") != "video/x-matroska" {
			t.Fatalf("part %d = %q (%v), want %q", i, got, err, want)
		}
	}
	if _, err := parts.NextPart(); err != io.EOF {
		t.Fatalf("end of multipart = %v, want EOF", err)
	}
	if b.closed.Load() != 1 {
		t.Fatalf("closed = %d, want 1", b.closed.Load())
	}
}

func TestStreamRejectsInvalidCapabilitiesAndRoutes(t *testing.T) {
	b := newStreamTestBackend(t)
	srv := newTestServer(t, b, enableStream)
	url := b.url(t, "")
	signature := b.signer.Sign(b.hash, b.filePath)
	badSignature := strings.Repeat("A", len(signature))
	for _, tc := range []struct {
		method string
		url    string
		status int
	}{
		{http.MethodGet, strings.Replace(url, signature, badSignature, 1), 403},
		{http.MethodGet, b.url(t, "missing.mkv"), 404},
		{http.MethodGet, "/stream", 404},
		{http.MethodGet, "/stream/bad", 404},
		{http.MethodGet, "/stream/", 404},
		{http.MethodPost, url, 405},
	} {
		rec := do(t, srv, httptest.NewRequest(tc.method, tc.url, nil))
		if rec.Code != tc.status {
			t.Fatalf("%s %s status = %d, want %d", tc.method, tc.url, rec.Code, tc.status)
		}
		if tc.status == 405 && rec.Header().Get("Allow") != "GET, HEAD" {
			t.Fatalf("Allow = %q", rec.Header().Get("Allow"))
		}
	}
	if b.opened.Load() != 0 || b.reads.Load() != 0 {
		t.Fatal("rejected request opened payload")
	}
	for _, tc := range []struct {
		name    string
		backend api.Backend
		tune    func(*config.Config)
	}{
		{name: "disabled", backend: b},
		{name: "management only", backend: &fakeBackend{}, tune: enableStream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, tc.backend, tc.tune)
			if got := do(t, s, httptest.NewRequest(http.MethodGet, url, nil)).Code; got != 404 {
				t.Fatalf("status = %d, want 404", got)
			}
		})
	}
}

func TestStreamLargeOffsetAndUnknownContentType(t *testing.T) {
	b := newStreamTestBackend(t)
	b.filePath = "large.unknown"
	b.size = 6 << 30
	b.read = func(_ context.Context, p []byte, off int64) (int, error) {
		for i := range p {
			p[i] = byte((off+int64(i))/251 + (off+int64(i))%251)
		}
		return len(p), nil
	}
	srv := newTestServer(t, b, enableStream)
	const start = int64(4<<30) + 123
	req := httptest.NewRequest(http.MethodGet, b.url(t, ""), nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+63))
	rec := do(t, srv, req)
	want := make([]byte, 64)
	_, _ = b.read(context.Background(), want, start)
	if rec.Code != 206 || !bytes.Equal(rec.Body.Bytes(), want) {
		t.Fatalf("large range = %d/%x, want 206/%x", rec.Code, rec.Body.Bytes(), want)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	if b.reads.Load() != 1 {
		t.Fatalf("reads = %d, want only the requested range", b.reads.Load())
	}
}

// A mid-file failure must abort the real transport, not finish with a plausible
// successful response or append a JSON error after some media bytes.
func TestStreamReadFailureAbortsTransport(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%v", http2), func(t *testing.T) {
			b := newStreamTestBackend(t)
			b.size = 1 << 20
			b.read = func(_ context.Context, p []byte, _ int64) (int, error) {
				n := min(len(p), 16<<10)
				copy(p[:n], bytes.Repeat([]byte{'x'}, n))
				return n, io.EOF
			}
			srv := newTestServer(t, b, enableStream)
			httpServer := httptest.NewUnstartedServer(srv.Handler())
			httpServer.EnableHTTP2 = http2
			httpServer.StartTLS()
			defer httpServer.Close()
			target := strings.Replace(b.url(t, ""), "http://example.test", httpServer.URL, 1)
			client := httpServer.Client()
			client.Timeout = 2 * time.Second
			response, err := client.Get(target)
			if err == nil {
				defer func() { _ = response.Body.Close() }()
				wantProto := 1
				if http2 {
					wantProto = 2
				}
				if response.ProtoMajor != wantProto {
					t.Fatalf("response protocol = %s", response.Proto)
				}
				body, readErr := io.ReadAll(response.Body)
				if readErr == nil {
					t.Fatalf("truncated stream completed successfully (%d bytes)", len(body))
				}
				if bytes.Contains(body, []byte("error")) {
					t.Fatalf("media body contains an appended error: %q", body)
				}
			}
			deadline := time.Now().Add(time.Second)
			for b.closed.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if b.closed.Load() != 1 {
				t.Fatalf("failed stream closed %d handles, want 1", b.closed.Load())
			}
		})
	}
}
