package session_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
	"github.com/yakumioto/torrentfs-go/internal/stream"
)

func streamLocationFor(t *testing.T, rawURL string) stream.Location {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	location, err := stream.ParsePath(parsed.EscapedPath())
	if err != nil {
		t.Fatal(err)
	}
	return location
}

// Signing needs metadata only; the HTTP endpoint must then read the exact
// requested bytes through the shared piece cache, not through a mounted path.
func TestStreamSessionSignedRangeAndLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := testConfig()
	cfg.STRM.BaseURL = "http://example.test"
	work := t.TempDir()
	name := "电影 %#?+.mkv"
	data := bytes.Repeat([]byte("real torrent stream content\n"), 30000)
	metainfoBytes, hash := buildSingleFileTorrentBytes(t, name, data, nil)
	sess := newLoopbackSession(t, cfg, filepath.Join(work, "torrents"))
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: metainfoBytes}); err != nil {
		t.Fatalf("AddTorrentAndPersist: %v", err)
	}
	link, err := sess.StreamURL(hash, name)
	if err != nil {
		t.Fatalf("StreamURL: %v", err)
	}
	torrent, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	if got := torrent.CachedBytes(); got != 0 {
		t.Fatalf("signing read %d payload bytes", got)
	}
	seedPieces(t, sess, hash, data)
	server, err := api.New(cfg, sess)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Shutdown(context.Background()) }()
	request := httptest.NewRequest(http.MethodGet, link, nil)
	const offset = testPieceLength + 123
	const length = 4096
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusPartialContent || !bytes.Equal(recorder.Body.Bytes(), data[offset:offset+length]) {
		t.Fatalf("ranged response = %d/%d bytes, want exact source range", recorder.Code, recorder.Body.Len())
	}

	location := streamLocationFor(t, link)
	bad := location
	bad.Signature = strings.Repeat("A", len(location.Signature))
	if _, err := sess.OpenStream(bad); !errors.Is(err, stream.ErrInvalidSignature) {
		t.Fatalf("tampered OpenStream = %v, want invalid signature", err)
	}
	if _, err := sess.StreamURL(hash, "missing.mkv"); !errors.Is(err, filesystem.ErrNotFound) {
		t.Fatalf("missing StreamURL = %v, want not found", err)
	}
	if _, err := sess.StreamURL(metainfo.Hash{}, name); !errors.Is(err, filesystem.ErrNotFound) {
		t.Fatalf("unknown StreamURL = %v, want not found", err)
	}
	if _, err := sess.CreateCategory(ctx, "电影"); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.SetTorrentCategory(ctx, hash.HexString(), "电影"); err != nil {
		t.Fatal(err)
	}
	categorized, err := sess.StreamURL(hash, name)
	if err != nil || categorized != link {
		t.Fatalf("category changed link: %v", err)
	}
	if err := sess.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := sess.OpenStream(location); !errors.Is(err, filesystem.ErrClosed) {
		t.Fatalf("closed OpenStream = %v, want closed", err)
	}
	restored := newLoopbackSession(t, cfg, filepath.Join(work, "torrents"))
	restarted, err := restored.StreamURL(hash, name)
	if err != nil || restarted != link {
		t.Fatalf("link changed after restart: %v", err)
	}
	seedPieces(t, restored, hash, data)
	file, err := restored.OpenStream(location)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if _, err := file.Reader.ReadAtContext(ctx, buf, 10); err != nil || !bytes.Equal(buf, data[10:74]) {
		t.Fatalf("restored read = %q, %v", buf, err)
	}
	if err := file.Reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.DeleteTorrent(ctx, hash.HexString()); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.OpenStream(location); !errors.Is(err, filesystem.ErrNotFound) {
		t.Fatalf("deleted OpenStream = %v, want not found", err)
	}
	if _, err := restored.StreamURL(hash, name); !errors.Is(err, filesystem.ErrNotFound) {
		t.Fatalf("deleted StreamURL = %v, want not found", err)
	}
}

func TestStreamDisabledDoesNotCreateKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "torrents")
	sess := newLoopbackSession(t, testConfig(), dir)
	if _, err := os.Lstat(filepath.Join(dir, ".metadata", "stream_key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled stream key exists: %v", err)
	}
	if _, err := sess.StreamURL(metainfo.Hash{}, "Movie.mkv"); !errors.Is(err, stream.ErrDisabled) {
		t.Fatalf("StreamURL = %v, want disabled", err)
	}
	if _, err := sess.OpenStream(stream.Location{}); !errors.Is(err, stream.ErrDisabled) {
		t.Fatalf("OpenStream = %v, want disabled", err)
	}
}

// A corrupt key fails before opening the client and must release the instance
// lock so fixing its permissions/content permits the next start.
func TestStreamCorruptKeyFailsStartupAndReleasesLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "torrents")
	metadata := filepath.Join(dir, ".metadata")
	if err := os.MkdirAll(metadata, 0o755); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(metadata, "stream_key")
	if err := os.WriteFile(keyPath, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.STRM.BaseURL = "http://example.test"
	if sess, err := session.New(cfg, dir); err == nil {
		_ = sess.Close(context.Background())
		t.Fatal("started with corrupt key")
	} else if !strings.Contains(err.Error(), "stream key") {
		t.Fatalf("startup error = %v, want stream key", err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	_ = newLoopbackSession(t, cfg, dir)
}

// A signed read has the same per-read timeout as FUSE, even if a torrent has
// metadata but no source. Releasing a stream must not cancel a different file.
func TestStreamPeerlessReadTimeoutAndCancellation(t *testing.T) {
	cfg := testConfig()
	cfg.STRM.BaseURL = "http://example.test"
	cfg.Mount.ReadTimeout = config.Duration(100 * time.Millisecond)
	sess := newLoopbackSession(t, cfg, filepath.Join(t.TempDir(), "torrents"))
	data := bytes.Repeat([]byte("no peer data"), 128)
	metainfoBytes, hash := buildSingleFileTorrentBytes(t, "Movie.mkv", data, nil)
	if err := sess.AddTorrent(context.Background(), session.Source{Metainfo: metainfoBytes}); err != nil {
		t.Fatal(err)
	}
	link, err := sess.StreamURL(hash, "Movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	file, err := sess.OpenStream(streamLocationFor(t, link))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Reader.Close() }()
	start := time.Now()
	if _, err := file.Reader.ReadAtContext(context.Background(), make([]byte, 64), 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("peerless read = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("peerless read took %s", elapsed)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := file.Reader.ReadAtContext(ctx, make([]byte, 64), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read = %v, want cancelled", err)
	}
	seedPieces(t, sess, hash, data)
	buf := make([]byte, 64)
	if _, err := file.Reader.ReadAtContext(context.Background(), buf, 0); err != nil && err != io.EOF {
		t.Fatalf("read after timeout = %v", err)
	}
	if !bytes.Equal(buf, data[:64]) {
		t.Fatal("timed out read damaged subsequent content")
	}
}
