package session_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestStorageUsesInjectedLogger(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := testTorrentDir(t, dataDir)
	content := []byte("storage payload")
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	torrentPath := filepath.Join(work, "payload.torrent")
	if err := os.WriteFile(torrentPath, torrentBytes, 0o644); err != nil {
		t.Fatalf("write torrent: %v", err)
	}
	payload := payloadDir(dataDir, hash)
	if err := os.MkdirAll(payload, 0o755); err != nil {
		t.Fatalf("make payload: %v", err)
	}
	if err := os.WriteFile(filepath.Join(payload, "payload.bin"), []byte("short"), 0o644); err != nil {
		t.Fatalf("write partial payload: %v", err)
	}

	var buf bytes.Buffer
	logger, _, err := logging.New(config.Log{Level: "warn", Format: "json"}, &buf)
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	sess, err := session.New(testConfig(dataDir), torrentsDir, session.WithLogger(logger))
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	if err := sess.AddTorrent(context.Background(), session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	select {
	case <-st.GotInfo():
	case <-time.After(5 * time.Second):
		t.Fatal("torrent info did not become available")
	}
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, `"level":"WARN"`) || !strings.Contains(output, `"msg":"file has unexpected size"`) {
		t.Fatalf("storage warning did not use injected JSON logger: %q", output)
	}
	if strings.Contains(output, "level=WARN") {
		t.Fatalf("storage warning used text logger: %q", output)
	}
}
