package session_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// TestSessionErrorPathsForUnknownTorrentAndFile pins the error identity the
// filesystem layer maps onto errnos: unknown torrents and missing display
// paths are ErrNotFound, not a crash or fabricated data.
func TestSessionErrorPathsForUnknownTorrentAndFile(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", []byte("error paths"))

	sess, err := session.New(testConfig(dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}

	if _, err := sess.OpenFile(metainfo.Hash{}, "payload.bin"); !errors.Is(err, filesystem.ErrNotFound) {
		t.Errorf("OpenFile(unknown torrent) = %v, want ErrNotFound", err)
	}
	if _, err := sess.OpenFile(hash, "does-not-exist.bin"); !errors.Is(err, filesystem.ErrNotFound) {
		t.Errorf("OpenFile(missing path) = %v, want ErrNotFound", err)
	}
	if _, err := sess.PieceStates(metainfo.Hash{}); !errors.Is(err, filesystem.ErrNotFound) {
		t.Errorf("PieceStates(unknown torrent) = %v, want ErrNotFound", err)
	}
}

// TestSessionIncompleteTorrentDoesNotFabricateData adds a torrent whose data
// is only partially present and which has no peers. A read must either fail or
// block; it must never report success with invented bytes, and Close must
// release any blocked reader.
func TestSessionIncompleteTorrentDoesNotFabricateData(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	tracker := newLoopbackTracker(t)

	content := make([]byte, 2*testPieceLength)
	for i := range content {
		content[i] = byte(i % 251)
	}
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, [][]string{{tracker.url}})
	torrentPath := filepath.Join(work, "payload.bin.torrent")
	if err := os.WriteFile(torrentPath, torrentBytes, 0o644); err != nil {
		t.Fatalf("write torrent: %v", err)
	}
	// Only half of the first piece exists on disk; nothing else is available
	// and no seeder ever joins the swarm.
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("make data dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "payload.bin"), content[:testPieceLength/2], 0o644); err != nil {
		t.Fatalf("write partial data: %v", err)
	}

	sess := newLoopbackSession(t, testConfig(dataDir))
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	select {
	case <-st.GotInfo():
	case <-ctx.Done():
		t.Fatalf("torrent info never arrived: %v", ctx.Err())
	}
	if st.BytesCompleted() == st.Length() {
		t.Fatal("torrent with partial data must not be complete")
	}

	// The piece state snapshot must show an incomplete torrent rather than
	// claiming every piece is present.
	states, err := sess.PieceStates(hash)
	if err != nil {
		t.Fatalf("PieceStates: %v", err)
	}
	complete := 0
	for _, state := range states {
		if state.Complete {
			complete++
		}
	}
	if complete == len(states) && len(states) > 0 {
		t.Fatalf("all %d pieces reported complete for a partial torrent", len(states))
	}

	ra, err := sess.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	readResult := make(chan error, 1)
	go func() {
		buf := make([]byte, 128)
		_, err := ra.ReadAt(buf, int64(testPieceLength)+64)
		readResult <- err
	}()

	select {
	case err := <-readResult:
		// Returning is allowed only as an error; success would mean invented
		// bytes were handed back.
		if err == nil {
			t.Error("read of missing data reported success")
		}
	case <-time.After(500 * time.Millisecond):
		// Still waiting on an unavailable piece: expected.
	}

	if err := sess.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-readResult:
	case <-time.After(15 * time.Second):
		t.Fatal("read did not unblock after Session.Close")
	}
}

// TestFuseMissingPathErrno checks the errnos a user sees for paths that do not
// exist in the mounted tree.
func TestFuseMissingPathErrno(t *testing.T) {
	requireFuse(t)
	ctx := testTimeout(t)

	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	mnt := filepath.Join(work, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatalf("make mountpoint: %v", err)
	}
	content := []byte("errno payload")
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", content)

	sess, err := session.New(testConfig(dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	waitComplete(t, ctx, st)

	server, err := filesystem.Mount(mnt, sess, nil)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer unmountServer(t, server, mnt)

	if _, err := os.Stat(filepath.Join(mnt, "no-such-torrent")); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("stat of unknown root entry = %v, want ENOENT", err)
	}
	if _, err := os.Stat(filepath.Join(mnt, "payload.bin", "no-such-file")); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("stat of missing torrent file = %v, want ENOENT", err)
	}
	if _, err := os.Open(filepath.Join(mnt, "payload.bin", "no-such-file")); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("open of missing torrent file = %v, want ENOENT", err)
	}
	// The torrent tree is read-only; writes must be refused rather than
	// silently accepted.
	if err := os.WriteFile(filepath.Join(mnt, "payload.bin", "payload.bin"), []byte("x"), 0o644); !errors.Is(err, syscall.EROFS) {
		t.Errorf("write into torrent tree = %v, want EROFS", err)
	}
}
