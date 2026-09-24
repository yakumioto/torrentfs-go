package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/cache"
)

func TestPlaybackStreamSeparatesReadDemandAndConsumption(t *testing.T) {
	store := cache.New(1 << 20)
	coordinator := newTestCoordinator(store)
	file := playbackTestFile(1024, 128, store, coordinator)

	started, err := coordinator.startPlaybackStream("stream-1", "payload.bin", file, 128)
	if err != nil {
		t.Fatalf("start stream: %v", err)
	}
	if started.PlaybackCursor != 128 || started.PlaybackConsumedBytes != 0 {
		t.Fatalf("start snapshot = %+v", started)
	}

	request, plan := playbackReadPlan(file, 256, 256)
	ticket := coordinator.beginForeground(file, request, plan, context.Background())
	if ticket == nil {
		t.Fatal("foreground ticket is nil")
	}
	ticket.startPiece(plan.Spans[0].Index)
	if len(ticket.pinned) != 0 {
		t.Fatalf("foreground pinned %d pieces before spans ran, want 0", len(ticket.pinned))
	}
	ticket.finishPiece(plan.Spans[0].Index)
	ticket.finish(256, 256, nil)

	snapshot := coordinator.playbackSnapshots()[0]
	if snapshot.PlaybackCursor != 128 || snapshot.PlaybackConsumedBytes != 0 {
		t.Fatalf("READ changed playback state = %+v", snapshot)
	}

	progress, err := coordinator.updatePlaybackStream("stream-1", PlaybackStreamUpdate{
		Sequence:      1,
		Event:         PlaybackEventProgress,
		PositionBytes: 192,
	})
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if progress.PlaybackCursor != 192 || progress.PlaybackConsumedBytes != 64 {
		t.Fatalf("progress snapshot = %+v", progress)
	}

	duplicate, err := coordinator.updatePlaybackStream("stream-1", PlaybackStreamUpdate{
		Sequence:      1,
		Event:         PlaybackEventProgress,
		PositionBytes: 192,
	})
	if err != nil || duplicate.PlaybackConsumedBytes != 64 {
		t.Fatalf("idempotent progress = %+v, err %v", duplicate, err)
	}
	if _, err := coordinator.updatePlaybackStream("stream-1", PlaybackStreamUpdate{
		Sequence:      1,
		Event:         PlaybackEventSeek,
		PositionBytes: 512,
	}); !errors.Is(err, ErrPlaybackSequenceConflict) {
		t.Fatalf("conflicting replay error = %v, want sequence conflict", err)
	}

	seek, err := coordinator.updatePlaybackStream("stream-1", PlaybackStreamUpdate{
		Sequence:      2,
		Event:         PlaybackEventSeek,
		PositionBytes: 768,
	})
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if seek.Generation != 2 || seek.PlaybackCursor != 768 || seek.PlaybackConsumedBytes != 64 {
		t.Fatalf("seek snapshot = %+v", seek)
	}
	if _, err := coordinator.updatePlaybackStream("stream-1", PlaybackStreamUpdate{
		Sequence:      3,
		Event:         PlaybackEventProgress,
		PositionBytes: 700,
	}); !errors.Is(err, ErrPlaybackPositionInvalid) {
		t.Fatalf("backward progress error = %v, want invalid position", err)
	}
}

func TestPlaybackStreamExpiresAndReleasesResources(t *testing.T) {
	oldLease := playbackLeaseDuration
	playbackLeaseDuration = time.Millisecond
	defer func() { playbackLeaseDuration = oldLease }()

	store := cache.New(1 << 20)
	coordinator := newTestCoordinator(store)
	coordinator.now = func() time.Time { return time.Unix(0, 0) }
	file := playbackTestFile(1024, 128, store, coordinator)
	if _, err := coordinator.startPlaybackStream("stream-1", "payload.bin", file, 0); err != nil {
		t.Fatalf("start stream: %v", err)
	}
	coordinator.now = func() time.Time { return time.Unix(1, 0) }
	coordinator.mu.Lock()
	coordinator.expirePlaybackStreamsLocked(coordinator.coordinatorNow())
	coordinator.mu.Unlock()
	if got := coordinator.playbackSnapshots(); len(got) != 0 {
		t.Fatalf("expired streams = %+v, want none", got)
	}
	if got := store.PinnedBytes(); got != 0 {
		t.Fatalf("pinned bytes after expiry = %d, want 0", got)
	}
}
