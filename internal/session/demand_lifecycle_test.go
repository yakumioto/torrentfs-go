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

func TestFullActiveWindowSharesOwnersAcrossStreams(t *testing.T) {
	store := cache.New(16 << 20)
	coordinator := newTestCoordinator(store)
	file := playbackTestFile(16<<20, 1<<20, store, coordinator)
	if _, err := coordinator.startPlaybackStream("stream-a", "payload.bin", file, 0); err != nil {
		t.Fatalf("start stream A: %v", err)
	}
	activeBefore := make(map[int]struct{}, len(coordinator.active))
	for index := range coordinator.active {
		activeBefore[index] = struct{}{}
	}
	if len(activeBefore) != defaultPrefetchPieces {
		t.Fatalf("stream A active pieces = %d, want %d", len(activeBefore), defaultPrefetchPieces)
	}
	_, budgetBefore := coordinator.budget.snapshot()
	if _, err := coordinator.startPlaybackStream("stream-b", "payload.bin", file, 0); err != nil {
		t.Fatalf("start stream B: %v", err)
	}
	if len(coordinator.active) != len(activeBefore) {
		t.Fatalf("stream B duplicated active leases: got %d, want %d", len(coordinator.active), len(activeBefore))
	}
	for index := range activeBefore {
		owners := coordinator.activeNormalOwners[index]
		if len(owners) != 2 {
			t.Fatalf("Piece %d active owners = %d, want 2", index, len(owners))
		}
	}
	if err := coordinator.stopPlaybackStream("stream-a"); err != nil {
		t.Fatalf("stop stream A: %v", err)
	}
	if len(coordinator.active) != len(activeBefore) {
		t.Fatalf("stream A stop cancelled shared active leases: got %d, want %d", len(coordinator.active), len(activeBefore))
	}
	for index := range activeBefore {
		owners := coordinator.activeNormalOwners[index]
		if len(owners) != 1 {
			t.Fatalf("Piece %d owners after stream A stop = %d, want 1", index, len(owners))
		}
	}
	if _, budgetAfterA := coordinator.budget.snapshot(); budgetAfterA != budgetBefore {
		t.Fatalf("budget after stream A stop = %d, want %d", budgetAfterA, budgetBefore)
	}
	if err := coordinator.stopPlaybackStream("stream-b"); err != nil {
		t.Fatalf("stop stream B: %v", err)
	}
	if len(coordinator.active) != 0 || len(coordinator.activeNormalOwners) != 0 {
		t.Fatalf("last stream stop left active=%d ownerSets=%d", len(coordinator.active), len(coordinator.activeNormalOwners))
	}
	if _, budgetAfterB := coordinator.budget.snapshot(); budgetAfterB != 0 {
		t.Fatalf("budget after last stream stop = %d, want 0", budgetAfterB)
	}
}

func TestNormalLeaseBookkeepingCoversCompletionBudgetAndStop(t *testing.T) {
	store := cache.New(16 << 20)
	coordinator := newTestCoordinator(store)
	file := playbackTestFile(8<<20, 1<<20, store, coordinator)
	if _, err := coordinator.startPlaybackStream("complete", "payload.bin", file, 0); err != nil {
		t.Fatalf("start completion stream: %v", err)
	}
	_, usedBefore := coordinator.budget.snapshot()
	if usedBefore == 0 || len(coordinator.active) == 0 {
		t.Fatalf("initial active state = active %d, budget %d", len(coordinator.active), usedBefore)
	}
	store.Put(cache.Key{Torrent: coordinator.torrentKey, Piece: 0}, make([]byte, 1<<20))
	coordinator.mu.Lock()
	coordinator.completeActiveLocked()
	coordinator.mu.Unlock()
	if _, active := coordinator.active[0]; active {
		t.Fatal("completed Piece remained active")
	}
	if _, activeOwner := coordinator.activeNormalOwners[0]; activeOwner {
		t.Fatal("completed Piece retained an active Normal owner")
	}
	_, usedAfter := coordinator.budget.snapshot()
	if usedAfter >= usedBefore {
		t.Fatalf("budget after completion = %d, before %d", usedAfter, usedBefore)
	}
	if err := coordinator.stopPlaybackStream("complete"); err != nil {
		t.Fatalf("stop completion stream: %v", err)
	}

	blocked := newTestCoordinator(cache.New(16 << 20))
	blockedFile := playbackTestFile(8<<20, 1<<20, blocked.cache, blocked)
	for i := 0; i < defaultPrefetchPieces; i++ {
		if !blocked.budget.tryAcquire() {
			t.Fatalf("reserve budget token %d", i)
		}
	}
	if _, err := blocked.startPlaybackStream("blocked", "payload.bin", blockedFile, 0); err != nil {
		t.Fatalf("start blocked stream: %v", err)
	}
	if len(blocked.active) != 0 || len(blocked.activeNormalOwners) != 0 {
		t.Fatalf("budget-blocked active state = active %d owners %d", len(blocked.active), len(blocked.activeNormalOwners))
	}
	for i := 0; i < defaultPrefetchPieces; i++ {
		blocked.budget.release()
	}
	_ = blocked.stopPlaybackStream("blocked")

	interleaved := newTestCoordinator(cache.New(16 << 20))
	interleavedFile := playbackTestFile(8<<20, 1<<20, interleaved.cache, interleaved)
	if _, err := interleaved.startPlaybackStream("interleaved", "payload.bin", interleavedFile, 0); err != nil {
		t.Fatalf("start interleaved stream: %v", err)
	}
	request, plan := playbackReadPlan(interleavedFile, 0, 64)
	ticket := interleaved.beginForeground(interleavedFile, request, plan, context.Background(), 404)
	ticket.startPiece(0)
	if err := interleaved.stopPlaybackStream("interleaved"); err != nil {
		t.Fatalf("stop interleaved stream: %v", err)
	}
	ticket.finishPiece(0)
	if len(interleaved.active) != 0 || len(interleaved.activeNormalOwners) != 0 {
		t.Fatalf("interleaved cleanup left active=%d owners=%d", len(interleaved.active), len(interleaved.activeNormalOwners))
	}
	if _, used := interleaved.budget.snapshot(); used != 0 {
		t.Fatalf("interleaved cleanup left budget token count %d", used)
	}
}
