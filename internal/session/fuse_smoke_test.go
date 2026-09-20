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

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// fuseSmokeProbe is the node type used for a throwaway mount that probes
// whether this environment can mount FUSE at all.
type fuseSmokeProbe struct{ fs.Inode }

// fuseRequired reports whether this run must exercise real FUSE mounts. CI
// sets it on the FUSE-enabled job so a missing device or capability fails the
// job instead of silently skipping every real-mount test.
func fuseRequired() bool {
	return os.Getenv("TORRENTFS_FUSE_REQUIRED") == "1"
}

// requireFuse skips the calling test when FUSE is unusable, or fails it when
// TORRENTFS_FUSE_REQUIRED demands a real mount. Either way the caller is told
// why, so an all-skip run cannot masquerade as a green FUSE job.
func requireFuse(t *testing.T) {
	t.Helper()
	if fuseUsable(t) {
		return
	}
	if fuseRequired() {
		t.Fatalf("TORRENTFS_FUSE_REQUIRED=1 but FUSE is not usable: /dev/fuse, fusermount, or mount permission is missing")
	}
	t.Skipf("FUSE not usable in this environment (/dev/fuse or mount permission missing); real-mount test skipped")
}

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
	requireFuse(t)

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
	torrentPath, hash := buildSingleFileTorrent(t, work, "payload.bin", content)

	sess, err := session.New(testConfig(), testTorrentDir(t, dataDir))
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
	seedPieces(t, sess, hash, content)
	waitCached(t, ctx, st)

	server, err := filesystem.Mount(mnt, sess, nil)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	// Best-effort cleanup so a mid-test failure does not leave a kernel mount
	// behind. The explicit Unmount below is the real assertion.
	defer func() { _ = server.Unmount() }()

	// Full read through the mount: a single-file torrent is exposed directly as
	// <mount>/<name>, a regular file a player can open and seek.
	path := filepath.Join(mnt, "payload.bin")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("%s mode = %v, want a regular file", path, info.Mode())
	}
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

	// The mount exposes data only: the former control directories are gone.
	for _, name := range []string{"metadata", "stats"} {
		if _, err := os.Stat(filepath.Join(mnt, name)); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("mount root %q = %v, want ENOENT", name, err)
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

// TestFuseReadOnlyDataTree checks a real mount exposes only the torrent data
// tree: the former metadata/ and stats/ control paths are absent, mutations at
// the root are refused, and the data itself still reads correctly.
func TestFuseReadOnlyDataTree(t *testing.T) {
	requireFuse(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	mnt := filepath.Join(work, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatalf("make mountpoint: %v", err)
	}
	files := map[string][]byte{
		"a.txt":     []byte("alpha file"),
		"sub/b.txt": []byte("beta file"),
	}
	torrentPath, hash, all := buildMultiFileTorrent(t, work, "multi", files)

	sess, err := session.New(testConfig(), testTorrentDir(t, dataDir))
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
		t.Fatal("torrent not registered")
	}
	seedPieces(t, sess, hash, all)
	waitCached(t, ctx, st)

	server, err := filesystem.Mount(mnt, sess, nil)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer func() { _ = server.Unmount() }()

	// The multi-file data tree is unchanged.
	root := filepath.Join(mnt, "multi")
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("multi root = (%v, %v), want directory", info, err)
	}
	for path, want := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		info, err := os.Stat(full)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("%s = (%v, %v), want regular file", path, info, err)
		}
		got, err := os.ReadFile(full)
		if err != nil || string(got) != string(want) {
			t.Fatalf("read %s = (%q, %v), want %q", path, got, err, want)
		}
	}

	// The control directories are no longer part of the mount.
	for _, name := range []string{"metadata", "stats"} {
		if _, err := os.Stat(filepath.Join(mnt, name)); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("mount root %q = %v, want ENOENT", name, err)
		}
	}
	// The old per-torrent .stats location no longer exists either.
	if _, err := os.Stat(filepath.Join(root, ".stats")); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("multi/.stats = %v, want ENOENT", err)
	}

	// Every mutation in the mount root is refused.
	if err := os.Mkdir(filepath.Join(mnt, "newdir"), 0o755); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("mkdir in mount root = %v, want EROFS", err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "new.torrent"), []byte("x"), 0o644); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("write in mount root = %v, want EROFS", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("write into torrent tree = %v, want EROFS", err)
	}
	if err := os.Remove(filepath.Join(root, "a.txt")); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("unlink in torrent tree = %v, want EROFS", err)
	}

	if err := server.Unmount(); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
}
