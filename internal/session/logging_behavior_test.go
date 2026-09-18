package session_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

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

func TestPieceCompletionPersistsAcrossSessionRestart(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := testTorrentDir(t, dataDir)

	first, err := session.New(testConfig(dataDir), torrentsDir)
	if err != nil {
		t.Fatalf("first session.New: %v", err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	payloadRoot := filepath.Join(dataDir, "payload")
	var completionPath string
	for _, name := range []string{".torrent.db", ".torrent.bolt.db"} {
		path := filepath.Join(payloadRoot, name)
		if _, err := os.Stat(path); err == nil {
			completionPath = path
			break
		}
	}
	if completionPath == "" {
		t.Fatalf("piece completion database was not created in %s", payloadRoot)
	}

	completion, err := storage.NewDefaultPieceCompletionForDir(payloadRoot)
	if err != nil {
		t.Fatalf("open piece completion: %v", err)
	}
	key := metainfo.PieceKey{InfoHash: metainfo.Hash{1}, Index: 0}
	if err := completion.Set(key, true); err != nil {
		_ = completion.Close()
		t.Fatalf("set piece completion: %v", err)
	}
	if err := completion.Close(); err != nil {
		t.Fatalf("close piece completion: %v", err)
	}

	second, err := session.New(testConfig(dataDir), torrentsDir)
	if err != nil {
		t.Fatalf("second session.New: %v", err)
	}
	if err := second.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	reopened, err := storage.NewDefaultPieceCompletionForDir(payloadRoot)
	if err != nil {
		t.Fatalf("reopen piece completion: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.Get(key)
	if err != nil {
		t.Fatalf("get piece completion: %v", err)
	}
	if !got.Ok || !got.Complete {
		t.Fatalf("piece completion after restart = %+v, want complete", got)
	}

	_ = completionPath
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
