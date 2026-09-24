package session

import (
	"context"
	"testing"
	"time"

	"github.com/anacrolix/torrent"

	"github.com/yakumioto/torrentfs-go/internal/cache"
)

func TestDemandCandidateCleanupIsolatedByHandle(t *testing.T) {
	store := cache.New(512 << 20)
	coordinator := newTestCoordinator(store)
	coordinator.torrent = &torrent.Torrent{}
	fileA := playbackTestFile(256<<20, 1<<20, store, coordinator)
	fileB := playbackTestFile(256<<20, 1<<20, store, coordinator)

	startA, planA := playbackReadPlan(fileA, 0, 64)
	firstA := coordinator.beginForeground(fileA, startA, planA, context.Background(), 101)
	firstA.finish(0, 64, nil)
	startB, planB := playbackReadPlan(fileB, 0, 64)
	firstB := coordinator.beginForeground(fileB, startB, planB, context.Background(), 202)
	firstB.finish(0, 64, nil)

	aRequest, aPlan := playbackReadPlan(fileA, 100<<20, 64)
	aOutstanding := coordinator.beginForeground(fileA, aRequest, aPlan, context.Background(), 101)
	bRequest, bPlan := playbackReadPlan(fileB, 100<<20, 64)
	bFirst := coordinator.beginForeground(fileB, bRequest, bPlan, context.Background(), 202)
	bFirst.finish(100<<20, 64, nil)
	bOutstanding := coordinator.beginForeground(fileB, bRequest, bPlan, context.Background(), 202)
	bSecondRequest, bSecondPlan := playbackReadPlan(fileB, (100<<20)+64, 64)
	bSecond := coordinator.beginForeground(fileB, bSecondRequest, bSecondPlan, context.Background(), 202)
	bSecond.finish((100<<20)+64, 64, nil)

	select {
	case <-aOutstanding.ctx.Done():
		t.Fatal("handle A candidate was cancelled by handle B confirmation")
	default:
	}
	select {
	case <-bOutstanding.ctx.Done():
	default:
		t.Fatal("handle B stale candidate remained after its own confirmation")
	}
	if aOutstanding.demandID == bOutstanding.demandID {
		t.Fatalf("demand IDs collided: %d", aOutstanding.demandID)
	}
}

func TestDemandIDsAreUniqueAcrossFiles(t *testing.T) {
	first := &raFile{}
	second := &raFile{}
	firstID, ok := first.acquireHandle()
	if !ok {
		t.Fatal("first handle acquisition failed")
	}
	secondID, ok := second.acquireHandle()
	if !ok {
		t.Fatal("second handle acquisition failed")
	}
	if firstID == secondID {
		t.Fatalf("demand IDs collided: %d", firstID)
	}
	first.releaseHandle()
	second.releaseHandle()
}

func TestPlaybackExpiryDetachesSessionStreamAndClosesHandle(t *testing.T) {
	oldLease := playbackLeaseDuration
	playbackLeaseDuration = time.Millisecond
	defer func() { playbackLeaseDuration = oldLease }()

	store := cache.New(1 << 20)
	coordinator := newTestCoordinator(store)
	coordinator.now = func() time.Time { return time.Unix(0, 0) }
	file := playbackTestFile(1024, 128, store, coordinator)
	file.handles = 1
	opened := &openedFile{file: file, demandID: 1}
	torrentHandle := &Torrent{coordinator: coordinator}
	stream := &playbackSessionStream{id: "expired", torrent: torrentHandle, file: opened}
	sess := &Session{playbackStreams: map[string]*playbackSessionStream{stream.id: stream}}
	if _, err := coordinator.startPlaybackStream(stream.id, "payload.bin", file, 0, func() {
		sess.removeExpiredPlaybackStream(stream.id, stream)
	}); err != nil {
		t.Fatalf("start stream: %v", err)
	}
	coordinator.now = func() time.Time { return time.Unix(1, 0) }
	coordinator.reconcile()

	sess.mu.RLock()
	_, stillRegistered := sess.playbackStreams[stream.id]
	sess.mu.RUnlock()
	if stillRegistered {
		t.Fatal("expired stream remained in Session registry without a follow-up request")
	}
	file.mu.RLock()
	handles := file.handles
	file.mu.RUnlock()
	if handles != 0 {
		t.Fatalf("expired stream left %d opened-file handles", handles)
	}
}

func TestDemandFallbackPinsOnlyCurrentPiece(t *testing.T) {
	store := cache.New(16 << 20)
	coordinator := newTestCoordinator(store)
	coordinator.demandOnly = true
	file := playbackTestFile(4<<20, 1<<20, store, coordinator)
	request, plan := playbackReadPlan(file, 0, 3<<20)
	ticket := coordinator.beginForeground(file, request, plan, context.Background(), 303)
	if ticket == nil {
		t.Fatal("foreground ticket is nil")
	}
	if len(ticket.pinned) != 0 {
		t.Fatalf("fallback pre-pinned %d Pieces for one READ", len(ticket.pinned))
	}
	for _, span := range plan.Spans {
		ticket.startPiece(span.Index)
		if got := len(coordinator.refs); got > 1 {
			t.Fatalf("foreground span %d retained %d Pieces", span.Index, got)
		}
		ticket.finishPiece(span.Index)
	}
	ticket.finish(0, 3<<20, nil)
	if got := len(coordinator.refs); got != 0 {
		t.Fatalf("fallback left %d Piece pins after READ", got)
	}
}
