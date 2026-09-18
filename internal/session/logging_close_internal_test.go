package session

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/logging"
)

type failingStorageCloser struct {
	err error
}

func (failingStorageCloser) OpenTorrent(context.Context, *metainfo.Info, metainfo.Hash) (storage.TorrentImpl, error) {
	return storage.TorrentImpl{}, nil
}

func (failingStorageCloser) Close() error { return failingStorageCloserErr }

var failingStorageCloserErr = errors.New("storage close failed")

func TestSessionCloseFailureHasNoCalleeErrorLog(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := filepath.Join(work, "torrents")
	if err := os.MkdirAll(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}

	var buf bytes.Buffer
	logger, _, err := logging.New(config.Log{Level: "info", Format: "text"}, &buf)
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	cfg := config.Default()
	cfg.Paths.DataDir = dataDir
	sess, err := New(cfg, torrentsDir, WithLogger(logger))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	originalStorage := sess.storageCloser
	sess.storageCloser = failingStorageCloser{}
	if err := originalStorage.Close(); err != nil {
		t.Fatalf("close original storage: %v", err)
	}

	if err := sess.Close(context.Background()); !errors.Is(err, failingStorageCloserErr) {
		t.Fatalf("Close error = %v, want storage close error", err)
	}
	output := buf.String()
	if strings.Contains(output, "session close failed") || strings.Contains(output, "session close stage failed") {
		t.Fatalf("callee emitted failure log: %q", output)
	}
	if strings.Contains(output, "session closed") {
		t.Fatalf("failed close emitted success log: %q", output)
	}
}
