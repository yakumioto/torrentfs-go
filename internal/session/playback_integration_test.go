package session_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

func TestExplicitPlaybackStreamUsesReportedPosition(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	sess := newLoopbackSession(t, testConfig(), filepath.Join(work, "data"))
	hash, _ := seedPrefetchTorrent(t, sess, "payload.bin", 16, 0)

	stream, err := sess.StartPlaybackStream(ctx, hash.HexString(), session.PlaybackStreamStart{
		Path:          "payload.bin",
		PositionBytes: 0,
	})
	if err != nil {
		t.Fatalf("StartPlaybackStream: %v", err)
	}
	if stream.ID == "" || stream.PlaybackCursor != 0 {
		t.Fatalf("start snapshot = %+v", stream)
	}

	ra, err := sess.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	closer, ok := ra.(interface{ Close() error })
	if !ok {
		t.Fatal("OpenFile did not return a closable handle")
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	opened := &prefetchFile{sess: sess, streamID: stream.ID, ra: ra, closer: closer}
	defer opened.close()
	opened.read(t, 0, 64)
	before := session.PrefetchSnapshotForTest(st)
	if len(before.PlaybackStreams) != 1 || before.PlaybackStreams[0].PlaybackCursor != 0 {
		t.Fatalf("READ changed playback stream = %+v", before.PlaybackStreams)
	}

	updated, err := sess.UpdatePlaybackStream(ctx, stream.ID, session.PlaybackStreamUpdate{
		Sequence:      1,
		Event:         session.PlaybackEventProgress,
		PositionBytes: 64,
	})
	if err != nil {
		t.Fatalf("UpdatePlaybackStream: %v", err)
	}
	if updated.PlaybackCursor != 64 || updated.PlaybackConsumedBytes != 64 {
		t.Fatalf("progress snapshot = %+v", updated)
	}
	if err := sess.StopPlaybackStream(context.Background(), stream.ID); err != nil {
		t.Fatalf("StopPlaybackStream: %v", err)
	}
}
