package session_test

import (
	"bytes"
	"context"
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
