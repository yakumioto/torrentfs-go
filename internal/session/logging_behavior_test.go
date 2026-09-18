package session_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/logging"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

func TestAddTorrentFailureIncludesStructuredContext(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := testTorrentDir(t, dataDir)
	missing := filepath.Join(work, "missing", "input.torrent")

	var buf bytes.Buffer
	logger, _, err := logging.New(config.Log{Level: "info", Format: "text"}, &buf)
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	sess, err := session.New(testConfig(dataDir), torrentsDir, session.WithLogger(logger))
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	if err := sess.AddTorrent(context.Background(), session.Source{MetainfoPath: missing}); err == nil {
		t.Fatal("AddTorrent succeeded for missing metainfo")
	}
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "level=ERROR") || !strings.Contains(output, "torrent add failed") {
		t.Fatalf("add failure log = %q", output)
	}
	if !strings.Contains(output, "err=") || !strings.Contains(output, "path=") {
		t.Fatalf("add failure log lacks context = %q", output)
	}
}

// TestPieceCacheIsNotRestoredAcrossRestart pins the core promise of the
// memory-only cache: a restart starts from an empty cache, nothing is read from
// disk, and no completion database is written.
func TestPieceCacheIsNotRestoredAcrossRestart(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := testTorrentDir(t, dataDir)
	content := []byte(strings.Repeat("memory only", 500))
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", content)

	first, err := session.New(testConfig(dataDir), torrentsDir)
	if err != nil {
		t.Fatalf("first session.New: %v", err)
	}
	if err := first.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("first AddTorrent: %v", err)
	}
	st, ok := first.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	seedPieces(t, first, hash, content)
	waitCached(t, ctx, st)
	if got := st.CachedBytes(); got != int64(len(content)) {
		t.Fatalf("cached bytes before restart = %d, want %d", got, len(content))
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// Nothing may have been written to disk: no payload tree and no piece
	// completion database.
	if _, err := os.Stat(filepath.Join(dataDir, "payload")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("payload dir exists after shutdown: %v", err)
	}
	var leftovers []string
	walkErr := filepath.WalkDir(dataDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch entry.Name() {
		case ".torrent.db", ".torrent.bolt.db":
			leftovers = append(leftovers, path)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk data dir: %v", walkErr)
	}
	if len(leftovers) != 0 {
		t.Fatalf("piece completion database files found after shutdown: %v", leftovers)
	}

	second, err := session.New(testConfig(dataDir), torrentsDir)
	if err != nil {
		t.Fatalf("second session.New: %v", err)
	}
	defer func() {
		if err := second.Close(context.Background()); err != nil {
			t.Errorf("second Close: %v", err)
		}
	}()
	if err := second.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("second AddTorrent: %v", err)
	}
	restored, ok := second.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered after restart")
	}
	if got := restored.CachedBytes(); got != 0 {
		t.Fatalf("cached bytes after restart = %d, want 0", got)
	}
}

func TestStorageUsesInjectedLogger(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := testTorrentDir(t, dataDir)
	content := []byte("storage logger payload")
	torrentBytes, _ := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	torrentPath := filepath.Join(work, "payload.torrent")
	if err := os.WriteFile(torrentPath, torrentBytes, 0o644); err != nil {
		t.Fatalf("write torrent: %v", err)
	}

	var buf bytes.Buffer
	logger, _, err := logging.New(config.Log{Level: "warn", Format: "json"}, &buf)
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	// A cache whose low-water mark is below the piece length makes the store
	// warn on open, which is the observable signal that the injected logger
	// reaches the piece store.
	cfg := testConfig(dataDir)
	cfg.Cache.CapacityBytes = testPieceLength + testPieceLength/8
	sess, err := session.New(cfg, torrentsDir, session.WithLogger(logger))
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	if err := sess.AddTorrent(context.Background(), session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, `"level":"WARN"`) || !strings.Contains(output, `"msg":"piece length exceeds cache low-water mark"`) {
		t.Fatalf("piece store warning did not use injected JSON logger: %q", output)
	}
	if strings.Contains(output, "level=WARN") {
		t.Fatalf("piece store warning used text logger: %q", output)
	}
}
