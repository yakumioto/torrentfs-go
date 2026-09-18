package session_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

const directorySyncTimeout = 5 * time.Second

func newDirectorySession(t *testing.T, dataDir, torrentsDir string) *session.Session {
	t.Helper()
	sess, err := session.New(testConfig(dataDir), torrentsDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return sess
}

func waitForTorrent(t *testing.T, sess *session.Session, hash metainfo.Hash) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), directorySyncTimeout)
	defer cancel()
	if err := waitFor(ctx, func() bool {
		_, ok := sess.Torrent(hash)
		return ok
	}); err != nil {
		t.Fatalf("wait for torrent %s: %v", hash.HexString(), err)
	}
}

func TestSessionRequiresExistingTorrentDirectory(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	missing := filepath.Join(work, "missing")

	if _, err := session.New(testConfig(dataDir), missing); err == nil {
		t.Fatal("New succeeded with a missing torrents directory")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing torrents directory after New = %v, want not exist", err)
	}

	file := filepath.Join(work, "input.torrent")
	if err := os.WriteFile(file, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if _, err := session.New(testConfig(dataDir), file); err == nil {
		t.Fatal("New succeeded with a regular-file torrents input")
	}
}

func TestSessionScansTorrentDirectoryAtStartup(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := filepath.Join(work, "torrents")
	if err := os.Mkdir(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	_, hash := buildSingleFileTorrent(t, dataDir, torrentsDir, "startup.bin", []byte("startup source"))

	sess := newDirectorySession(t, dataDir, torrentsDir)
	waitForTorrent(t, sess, hash)
}

func TestSessionSynchronizesTorrentDirectoryAddAndRemove(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := filepath.Join(work, "torrents")
	if err := os.Mkdir(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	sess := newDirectorySession(t, dataDir, torrentsDir)

	path, hash := buildSingleFileTorrent(t, dataDir, torrentsDir, "dynamic.bin", []byte("dynamic source"))
	waitForTorrent(t, sess, hash)

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove torrent source: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), directorySyncTimeout)
	defer cancel()
	if err := waitFor(ctx, func() bool {
		_, ok := sess.Torrent(hash)
		return !ok
	}); err != nil {
		t.Fatalf("wait for source removal: %v", err)
	}
}

func TestSessionIgnoresNonTorrentDirectoryEntries(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := filepath.Join(work, "torrents")
	if err := os.Mkdir(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	bytes, hash := buildSingleFileTorrentBytes(t, "ignored.bin", []byte("ignored source"), nil)
	outside := filepath.Join(work, "outside.torrent")
	if err := os.WriteFile(outside, bytes, 0o644); err != nil {
		t.Fatalf("write outside torrent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(torrentsDir, "upper.TORRENT"), bytes, 0o644); err != nil {
		t.Fatalf("write upper extension: %v", err)
	}
	if err := os.WriteFile(filepath.Join(torrentsDir, "partial.torrent.part"), bytes, 0o644); err != nil {
		t.Fatalf("write partial extension: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(torrentsDir, "link.torrent")); err != nil {
		t.Fatalf("make symlink: %v", err)
	}
	if err := os.Mkdir(filepath.Join(torrentsDir, "directory.torrent"), 0o755); err != nil {
		t.Fatalf("make torrent-named directory: %v", err)
	}
	if err := os.Mkdir(filepath.Join(torrentsDir, "nested"), 0o755); err != nil {
		t.Fatalf("make nested directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(torrentsDir, "nested", "nested.torrent"), bytes, 0o644); err != nil {
		t.Fatalf("write nested torrent: %v", err)
	}
	if err := os.Mkdir(filepath.Join(torrentsDir, ".metadata"), 0o755); err != nil {
		t.Fatalf("make metadata directory: %v", err)
	}

	sess := newDirectorySession(t, dataDir, torrentsDir)
	if got := len(sess.List()); got != 0 {
		t.Fatalf("List after ignored entries = %d, want 0", got)
	}
	if _, ok := sess.Torrent(hash); ok {
		t.Fatal("ignored torrent was registered")
	}

	path := filepath.Join(torrentsDir, "valid.torrent")
	if err := os.WriteFile(path, bytes, 0o644); err != nil {
		t.Fatalf("write valid source: %v", err)
	}
	waitForTorrent(t, sess, hash)
}

func TestSessionWaitsForStableTorrentFile(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := filepath.Join(work, "torrents")
	if err := os.Mkdir(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	bytes, hash := buildSingleFileTorrentBytes(t, "stable.bin", []byte("stable source"), nil)
	sess := newDirectorySession(t, dataDir, torrentsDir)
	path := filepath.Join(torrentsDir, "incoming.torrent")
	if err := os.WriteFile(path, bytes[:len(bytes)/2], 0o644); err != nil {
		t.Fatalf("write partial torrent: %v", err)
	}

	timer := time.NewTimer(350 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	if _, ok := sess.Torrent(hash); ok {
		t.Fatal("partially written torrent was registered")
	}

	if err := os.WriteFile(path, bytes, 0o644); err != nil {
		t.Fatalf("finish torrent: %v", err)
	}
	waitForTorrent(t, sess, hash)
}

func TestSessionRetainsOldSourceUntilReplacementIsValid(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := filepath.Join(work, "torrents")
	if err := os.Mkdir(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	first, firstHash := buildSingleFileTorrentBytes(t, "first.bin", []byte("first source"), nil)
	second, secondHash := buildSingleFileTorrentBytes(t, "second.bin", []byte("second source"), nil)
	path := filepath.Join(torrentsDir, "source.torrent")
	if err := os.WriteFile(path, first, 0o644); err != nil {
		t.Fatalf("write first source: %v", err)
	}
	sess := newDirectorySession(t, dataDir, torrentsDir)
	waitForTorrent(t, sess, firstHash)

	if err := os.WriteFile(path, []byte("broken replacement"), 0o644); err != nil {
		t.Fatalf("write broken replacement: %v", err)
	}
	time.Sleep(350 * time.Millisecond)
	if _, ok := sess.Torrent(firstHash); !ok {
		t.Fatal("invalid replacement removed the previously valid torrent")
	}

	temporary := filepath.Join(torrentsDir, "source.torrent.part")
	if err := os.WriteFile(temporary, second, 0o644); err != nil {
		t.Fatalf("write replacement temporary: %v", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		t.Fatalf("atomically replace source: %v", err)
	}
	waitForTorrent(t, sess, secondHash)
	ctx, cancel := context.WithTimeout(context.Background(), directorySyncTimeout)
	defer cancel()
	if err := waitFor(ctx, func() bool {
		_, ok := sess.Torrent(firstHash)
		return !ok
	}); err != nil {
		t.Fatalf("wait for replaced source removal: %v", err)
	}
}

func TestSessionKeepsDuplicateDirectoryAndMetadataReferences(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := filepath.Join(work, "torrents")
	if err := os.Mkdir(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	bytes, hash := buildSingleFileTorrentBytes(t, "shared.bin", []byte("shared source"), nil)
	first := filepath.Join(torrentsDir, "first.torrent")
	second := filepath.Join(torrentsDir, "second.torrent")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, bytes, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	sess := newDirectorySession(t, dataDir, torrentsDir)
	waitForTorrent(t, sess, hash)

	if err := os.Remove(first); err != nil {
		t.Fatalf("remove first source: %v", err)
	}
	time.Sleep(350 * time.Millisecond)
	if _, ok := sess.Torrent(hash); !ok {
		t.Fatal("removing one duplicate source dropped the torrent")
	}

	// A managed internal source keeps the task alive after the user's own
	// .torrent file is gone.
	persistManaged(t, sess, bytes)
	if err := os.Remove(second); err != nil {
		t.Fatalf("remove second source: %v", err)
	}
	time.Sleep(350 * time.Millisecond)
	if _, ok := sess.Torrent(hash); !ok {
		t.Fatal("managed metadata reference did not preserve the torrent")
	}

	ctx, cancel := context.WithTimeout(context.Background(), directorySyncTimeout)
	defer cancel()
	op, err := sess.DeleteTorrent(ctx, hash.HexString())
	if err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}
	waitOperationDone(t, ctx, sess, op.ID)
	if _, ok := sess.Torrent(hash); ok {
		t.Fatal("deleted torrent is still registered")
	}
}

func TestSessionUsesOnlyMetadataInsideTorrentDirectory(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := filepath.Join(work, "torrents")
	if err := os.Mkdir(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	bytes, hash := buildSingleFileTorrentBytes(t, "metadata.bin", []byte("metadata source"), nil)
	oldMetadataDir := filepath.Clean(dataDir) + ".metadata"
	if err := os.MkdirAll(oldMetadataDir, 0o755); err != nil {
		t.Fatalf("make legacy metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(oldMetadataDir, "legacy.torrent"), bytes, 0o644); err != nil {
		t.Fatalf("write legacy metadata: %v", err)
	}

	first := newDirectorySession(t, dataDir, torrentsDir)
	if got := len(first.List()); got != 0 {
		t.Fatalf("legacy metadata was restored: List = %d, want 0", got)
	}
	metadataDir := filepath.Join(torrentsDir, ".metadata")
	if info, err := os.Stat(metadataDir); err != nil || !info.IsDir() {
		t.Fatalf("new metadata dir = (%v, %v), want directory", info, err)
	}
	persistManaged(t, first, bytes)
	canonical := filepath.Join(metadataDir, hash.HexString()+".torrent")
	if _, err := os.Stat(canonical); err != nil {
		t.Fatalf("canonical metadata file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(oldMetadataDir, "current.torrent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy metadata directory was written: %v", err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second := newDirectorySession(t, dataDir, torrentsDir)
	waitForTorrent(t, second, hash)
	if _, err := os.Stat(canonical); err != nil {
		t.Fatalf("canonical metadata file lost after restart: %v", err)
	}
}

func TestSessionCloseRacesTorrentDirectorySynchronization(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := filepath.Join(work, "torrents")
	if err := os.Mkdir(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	bytes, _ := buildSingleFileTorrentBytes(t, "race.bin", []byte("race source"), nil)
	sess := newDirectorySession(t, dataDir, torrentsDir)

	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		path := filepath.Join(torrentsDir, "race.torrent")
		temporary := filepath.Join(torrentsDir, "race.torrent.part")
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.WriteFile(temporary, bytes, 0o644)
			_ = os.Rename(temporary, path)
			_ = os.Remove(path)
		}
	}()

	time.Sleep(150 * time.Millisecond)
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(stop)
	select {
	case <-writerDone:
	case <-time.After(directorySyncTimeout):
		t.Fatal("torrent directory writer did not stop")
	}
}
