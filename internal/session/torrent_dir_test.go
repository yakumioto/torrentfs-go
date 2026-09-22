package session_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

func TestSessionRequiresExistingTorrentDirectory(t *testing.T) {
	work := t.TempDir()
	missing := filepath.Join(work, "missing")

	if _, err := session.New(testConfig(), missing); err == nil {
		t.Fatal("New succeeded with a missing torrents directory")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing torrents directory after New = %v, want not exist", err)
	}

	file := filepath.Join(work, "input.torrent")
	if err := os.WriteFile(file, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if _, err := session.New(testConfig(), file); err == nil {
		t.Fatal("New succeeded with a regular-file torrents input")
	}
}

func TestSessionIgnoresRootTorrentAtStartup(t *testing.T) {
	work := t.TempDir()
	torrentsDir := filepath.Join(work, "torrents")
	if err := os.Mkdir(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	path, hash := buildSingleFileTorrent(t, torrentsDir, "startup.bin", []byte("startup source"))

	sess, err := session.New(testConfig(), torrentsDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if _, ok := sess.Torrent(hash); ok {
		t.Fatal("manual root torrent was registered")
	}
	if len(sess.ListTorrents()) != 0 {
		t.Fatalf("manual root torrent appeared in registry view: %v", sess.ListTorrents())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("manual root torrent disappeared: %v", err)
	}
	if _, err := os.Stat(filepath.Join(torrentsDir, ".metadata", "layout_version")); err != nil {
		t.Fatalf("layout marker missing: %v", err)
	}
}

func TestSessionIgnoresRootTorrentAfterLayoutMarker(t *testing.T) {
	work := t.TempDir()
	torrentsDir := filepath.Join(work, "torrents")
	if err := os.Mkdir(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	first, err := session.New(testConfig(), torrentsDir)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	_, hash := buildSingleFileTorrentBytes(t, "after-marker.bin", []byte("after marker"), nil)
	bytes, _ := buildSingleFileTorrentBytes(t, "after-marker.bin", []byte("after marker"), nil)
	if err := os.WriteFile(filepath.Join(torrentsDir, hash.HexString()+".torrent"), bytes, 0o644); err != nil {
		t.Fatalf("write canonical-looking orphan: %v", err)
	}

	second, err := session.New(testConfig(), torrentsDir)
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer func() {
		if err := second.Close(context.Background()); err != nil {
			t.Errorf("second Close: %v", err)
		}
	}()
	if _, ok := second.Torrent(hash); ok {
		t.Fatal("orphan written after marker was registered")
	}
	if len(second.ListTorrents()) != 0 {
		t.Fatalf("orphan written after marker appeared in registry: %v", second.ListTorrents())
	}
}

func TestSessionRejectsMetadataSubdirectorySymlink(t *testing.T) {
	work := t.TempDir()
	torrentsDir := filepath.Join(work, "torrents")
	if err := os.Mkdir(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	metadataDir := filepath.Join(torrentsDir, ".metadata")
	if err := os.Mkdir(metadataDir, 0o755); err != nil {
		t.Fatalf("make metadata dir: %v", err)
	}
	if err := os.Symlink(filepath.Join(work, "outside"), filepath.Join(metadataDir, "pending")); err != nil {
		t.Fatalf("make pending symlink: %v", err)
	}
	if _, err := session.New(testConfig(), torrentsDir); err == nil {
		t.Fatal("New accepted a pending directory symlink")
	}
}
