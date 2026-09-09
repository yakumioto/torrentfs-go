package session_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// fuseSmokeProbe is the node type used for a throwaway mount that probes
// whether this environment can mount FUSE at all.
type fuseSmokeProbe struct{ fs.Inode }

// fuseUsable reports whether this environment can mount a FUSE filesystem.
// A missing /dev/fuse, a missing fusermount binary, or a mount refused by the
// kernel (common in unprivileged containers) all mean "not usable"; the smoke
// test then skips instead of failing.
func fuseUsable(t *testing.T) bool {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		return false
	}
	mnt, err := os.MkdirTemp("", "torrentfs-fuse-probe-*")
	if err != nil {
		t.Fatalf("make probe mountpoint: %v", err)
	}
	defer func() { _ = os.Remove(mnt) }()
	server, err := fs.Mount(mnt, &fuseSmokeProbe{}, &fs.Options{
		MountOptions: fuse.MountOptions{Options: []string{"ro"}},
	})
	if err != nil {
		return false
	}
	if err := server.Unmount(); err != nil {
		return false
	}
	return true
}

// TestFuseSmokeMountsAndReads mounts a real torrent and reads its content
// through the mounted tree, both a full read and a seeked read. It only runs
// where FUSE is available; elsewhere it skips with a reason and is not a green
// result.
func TestFuseSmokeMountsAndReads(t *testing.T) {
	if !fuseUsable(t) {
		t.Skipf("FUSE not usable in this environment (/dev/fuse or mount permission missing); real-mount smoke test skipped")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	work := t.TempDir()
	dataDir := work + "/data"
	mnt := work + "/mnt"
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatalf("make mountpoint: %v", err)
	}

	content := make([]byte, 4096)
	for i := range content {
		content[i] = byte('a' + i%26)
	}
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", content)

	sess, err := session.New(config.Config{Paths: config.Paths{DataDir: dataDir}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatalf("torrent %s not registered", metainfo.Hash(hash))
	}
	waitComplete(t, ctx, st)

	server, err := filesystem.Mount(mnt, sess, nil)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	// Best-effort cleanup so a mid-test failure does not leave a kernel mount
	// behind. The explicit Unmount below is the real assertion.
	defer func() { _ = server.Unmount() }()

	// Full read through the mount: <mount>/<torrent>/<file>.
	path := filepath.Join(mnt, "payload.bin", "payload.bin")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if len(got) != len(content) {
		t.Fatalf("read %d bytes, want %d", len(got), len(content))
	}
	for i := range content {
		if got[i] != content[i] {
			t.Fatalf("byte %d = %q, want %q", i, got[i], content[i])
		}
	}

	// Seeked read: read a window from the middle of the mounted file.
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(100, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	window := make([]byte, 128)
	if _, err := io.ReadFull(f, window); err != nil {
		t.Fatalf("seeked read: %v", err)
	}
	for i := range window {
		if window[i] != content[100+i] {
			t.Fatalf("seeked byte %d = %q, want %q", i, window[i], content[100+i])
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close mounted file: %v", err)
	}

	// Nothing holds the mount open now; unmount must succeed.
	if err := server.Unmount(); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
}

func TestFuseMetadataLifecycle(t *testing.T) {
	if !fuseUsable(t) {
		t.Skipf("FUSE not usable in this environment (/dev/fuse or mount permission missing); metadata integration test skipped")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	mnt := filepath.Join(work, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatalf("make mountpoint: %v", err)
	}
	content := []byte("metadata lifecycle data")
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", content)
	otherContent := []byte("different metadata lifecycle data")
	otherTorrentPath, otherHash := buildSingleFileTorrent(t, dataDir, work, "other.bin", otherContent)
	torrentBytes, err := os.ReadFile(torrentPath)
	if err != nil {
		t.Fatalf("read torrent: %v", err)
	}
	otherTorrentBytes, err := os.ReadFile(otherTorrentPath)
	if err != nil {
		t.Fatalf("read other torrent: %v", err)
	}

	sess, err := session.New(config.Config{Paths: config.Paths{DataDir: dataDir}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	server, err := filesystem.Mount(mnt, sess, nil)
	if err != nil {
		_ = sess.Close(context.Background())
		t.Fatalf("Mount: %v", err)
	}
	unmounted, closed := false, false
	defer func() {
		if !unmounted {
			_ = server.Unmount()
		}
		if !closed {
			_ = sess.Close(context.Background())
		}
	}()

	metadataDir := filepath.Join(mnt, "metadata")
	first := filepath.Join(metadataDir, "first.torrent")
	second := filepath.Join(metadataDir, "second.torrent")
	renamed := filepath.Join(metadataDir, "renamed.torrent")
	if err := os.WriteFile(first, torrentBytes, 0o644); err != nil {
		t.Fatalf("write first metadata: %v", err)
	}
	if err := waitFor(ctx, func() bool {
		_, ok := sess.Torrent(hash)
		return ok
	}); err != nil {
		t.Fatalf("wait for first metadata add: %v", err)
	}
	if err := os.WriteFile(second, torrentBytes, 0o644); err != nil {
		t.Fatalf("write second metadata: %v", err)
	}
	if err := waitFor(ctx, func() bool { return len(sess.MetadataFiles()) == 2 }); err != nil {
		t.Fatalf("wait for metadata files: %v", err)
	}

	if err := os.Rename(second, first); !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("rename over existing metadata = %v, want EEXIST", err)
	}
	firstInfo, err := os.Stat(first)
	if err != nil {
		t.Fatalf("stat original metadata: %v", err)
	}
	if err := os.Rename(first, renamed); err != nil {
		t.Fatalf("rename metadata: %v", err)
	}
	renamedInfo, err := os.Stat(renamed)
	if err != nil {
		t.Fatalf("stat renamed FUSE path: %v", err)
	}
	if !os.SameFile(firstInfo, renamedInfo) {
		t.Fatal("rename changed the metadata inode")
	}
	if got, err := os.ReadFile(renamed); err != nil || string(got) != string(torrentBytes) {
		t.Fatalf("read renamed metadata: bytes=%d err=%v", len(got), err)
	}

	if err := os.WriteFile(first, otherTorrentBytes, 0o644); err != nil {
		t.Fatalf("recreate original metadata name: %v", err)
	}
	if err := waitFor(ctx, func() bool { return len(sess.MetadataFiles()) == 3 }); err != nil {
		t.Fatalf("wait for recreated metadata: %v", err)
	}
	newFirstInfo, err := os.Stat(first)
	if err != nil {
		t.Fatalf("stat recreated metadata: %v", err)
	}
	if os.SameFile(firstInfo, newFirstInfo) || os.SameFile(renamedInfo, newFirstInfo) {
		t.Fatal("recreated metadata reused the renamed inode")
	}
	if got, err := os.ReadFile(first); err != nil || string(got) != string(otherTorrentBytes) {
		t.Fatalf("read recreated metadata: bytes=%d err=%v", len(got), err)
	}

	if err := os.Rename(second, renamed); !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("rename over renamed metadata = %v, want EEXIST", err)
	}
	if err := os.Remove(metadataDir); !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatalf("rmdir non-empty metadata = %v, want ENOTEMPTY", err)
	}
	if err := os.Remove(renamed); err != nil {
		t.Fatalf("unlink renamed metadata: %v", err)
	}
	if err := os.Remove(second); err != nil {
		t.Fatalf("unlink second metadata: %v", err)
	}
	if err := os.Remove(first); err != nil {
		t.Fatalf("unlink recreated metadata: %v", err)
	}
	if err := waitFor(ctx, func() bool {
		if _, ok := sess.Torrent(hash); ok {
			return false
		}
		_, ok := sess.Torrent(otherHash)
		return !ok
	}); err != nil {
		t.Fatalf("wait for metadata removal: %v", err)
	}
	if err := os.Remove(metadataDir); err != nil {
		t.Fatalf("rmdir empty metadata: %v", err)
	}
	if err := os.Mkdir(metadataDir, 0o755); err != nil {
		t.Fatalf("mkdir metadata after rmdir: %v", err)
	}

	if err := server.Unmount(); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	unmounted = true
	if err := sess.Close(ctx); err != nil {
		t.Fatalf("Close after unmount: %v", err)
	}
	closed = true
}

func waitFor(ctx context.Context, condition func() bool) error {
	if condition() {
		return nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if condition() {
				return nil
			}
		}
	}
}
