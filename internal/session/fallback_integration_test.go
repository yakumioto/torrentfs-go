package session_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

func TestReadWithoutPlaybackStreamIsForegroundOnly(t *testing.T) {
	ctx := testTimeout(t)
	sess := newLoopbackSession(t, testConfig(), filepath.Join(t.TempDir(), "data"))
	hash, _ := seedPrefetchTorrent(t, sess, "payload.bin", 16, 0)
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	reader, err := sess.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	closer, ok := reader.(interface{ Close() error })
	if !ok {
		t.Fatal("OpenFile did not return a closable handle")
	}
	defer func() {
		if err := closer.Close(); err != nil {
			t.Errorf("close fallback handle: %v", err)
		}
	}()

	buf := make([]byte, 64)
	if _, err := reader.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if err := waitFor(ctx, func() bool {
		snapshot := session.PrefetchSnapshotForTest(st)
		return snapshot.ForegroundTickets == 0
	}); err != nil {
		t.Fatalf("foreground request did not finish: %v", err)
	}
	snapshot := session.PrefetchSnapshotForTest(st)
	if snapshot.ActivePieces != 0 || snapshot.PinnedPieces != 0 || snapshot.PlaybackCursor != 0 || len(snapshot.PlaybackStreams) != 0 {
		t.Fatalf("no-stream fallback created scheduler state: %+v", snapshot)
	}
}

func TestDirectReadAtHandleCloseCancelsOwnDemand(t *testing.T) {
	ctx := testTimeout(t)
	sess := newLoopbackSession(t, testConfig(), filepath.Join(t.TempDir(), "data"))
	hash, _ := seedPrefetchTorrent(t, sess, "payload.bin", 16, 0)
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	readerA, err := sess.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatalf("OpenFile A: %v", err)
	}
	readerB, err := sess.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatalf("OpenFile B: %v", err)
	}
	closeA := readerA.(interface{ Close() error })
	closeB := readerB.(interface{ Close() error })
	defer func() {
		if err := closeB.Close(); err != nil {
			t.Errorf("close reader B: %v", err)
		}
	}()

	readDone := make(chan error, 1)
	go func() {
		_, readErr := readerA.ReadAt(make([]byte, 64), 8*testPieceLength)
		readDone <- readErr
	}()
	if err := waitFor(ctx, func() bool {
		return session.PrefetchSnapshotForTest(st).ForegroundTickets > 0
	}); err != nil {
		t.Fatalf("direct ReadAt did not enter foreground demand: %v", err)
	}
	if err := closeA.Close(); err != nil {
		t.Fatalf("close A: %v", err)
	}
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("closing direct ReadAt handle did not cancel its demand")
	}
	if _, err := readerB.ReadAt(make([]byte, 64), 0); err != nil {
		t.Fatalf("handle B read after A close: %v", err)
	}
}
