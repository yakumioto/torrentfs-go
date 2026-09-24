package session_test

import (
	"path/filepath"
	"testing"

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
	defer closer.Close()

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
