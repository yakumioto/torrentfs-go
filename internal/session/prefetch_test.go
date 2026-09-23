package session

import (
	"context"
	"io"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/cache"
)

func TestPrefetchBudgetIsSessionGlobal(t *testing.T) {
	budget := newPrefetchBudget(defaultPrefetchPieces)
	if limit, used := budget.snapshot(); limit != defaultPrefetchPieces || used != 0 {
		t.Fatalf("new budget = (%d, %d), want (%d, 0)", limit, used, defaultPrefetchPieces)
	}
	for i := 0; i < defaultPrefetchPieces; i++ {
		if !budget.tryAcquire() {
			t.Fatalf("acquire %d of %d was refused", i+1, defaultPrefetchPieces)
		}
	}
	if budget.tryAcquire() {
		t.Fatalf("acquired %d tokens, want the limit of %d to hold", defaultPrefetchPieces+1, defaultPrefetchPieces)
	}
	if _, used := budget.snapshot(); used != defaultPrefetchPieces {
		t.Fatalf("used = %d after a refused acquire, want %d", used, defaultPrefetchPieces)
	}
	budget.release()
	if !budget.tryAcquire() {
		t.Fatal("a released token was not reusable")
	}

	restore := budget.setLimit(6)
	if limit, _ := budget.snapshot(); limit != 6 {
		t.Fatalf("limit = %d after setLimit(6), want 6", limit)
	}
	restore()
	if limit, _ := budget.snapshot(); limit != defaultPrefetchPieces {
		t.Fatalf("limit = %d after restore, want %d", limit, defaultPrefetchPieces)
	}
}

// newTestCoordinator builds a coordinator that never touches a torrent. Only
// the cache-facing and anchor-facing helpers may be called on it.
func newTestCoordinator(c *cache.Cache) *prefetchCoordinator {
	return &prefetchCoordinator{
		cache:            c,
		budget:           newPrefetchBudget(defaultPrefetchPieces),
		torrentKey:       "prefetch-test",
		refs:             make(map[int]int),
		windowPins:       make(map[int]struct{}),
		active:           make(map[int]struct{}),
		foreground:       make(map[uint64]*foregroundTicket),
		foregroundPieces: make(map[int]int),
	}
}

func TestPrefetchBufferedBytesCountsOnlyContiguousVerifiedResident(t *testing.T) {
	const pieceLength = int64(100)
	const fileSize = int64(500)
	c := newTestCoordinator(cache.New(1 << 20))
	c.hasAnchor = true
	c.anchor = prefetchAnchor{
		fileStart:   0,
		fileSize:    fileSize,
		pieceLength: pieceLength,
		torrentSize: fileSize,
		cursor:      150, // mid-way through piece 1
	}

	// Pieces 1 and 2 are resident; piece 3 is missing, so only the contiguous
	// run from the cursor counts: 50 bytes of piece 1 plus piece 2.
	c.cache.Put(c.key(1), make([]byte, pieceLength))
	c.cache.Put(c.key(2), make([]byte, pieceLength))
	c.cache.Put(c.key(4), make([]byte, pieceLength))
	c.updateBufferedLocked()
	if want := int64(50) + pieceLength; c.bufferedBytes != want {
		t.Fatalf("bufferedBytes = %d, want %d (gap at piece 3 must stop the count)", c.bufferedBytes, want)
	}

	// Filling piece 3 extends the run through piece 4.
	c.cache.Put(c.key(3), make([]byte, pieceLength))
	c.updateBufferedLocked()
	if want := int64(50) + 3*pieceLength; c.bufferedBytes != want {
		t.Fatalf("bufferedBytes = %d, want %d after the gap closed", c.bufferedBytes, want)
	}
}

func TestPrefetchBufferedBytesClampsAtFileEnd(t *testing.T) {
	const pieceLength = int64(100)
	const fileSize = int64(250) // last piece is a 50-byte short piece
	c := newTestCoordinator(cache.New(1 << 20))
	c.hasAnchor = true
	c.anchor = prefetchAnchor{
		fileStart:   0,
		fileSize:    fileSize,
		pieceLength: pieceLength,
		torrentSize: fileSize,
		cursor:      0,
	}
	for index := 0; index < 3; index++ {
		length := pieceLength
		if index == 2 {
			length = 50
		}
		c.cache.Put(c.key(index), make([]byte, length))
	}
	c.updateBufferedLocked()
	if c.bufferedBytes != fileSize {
		t.Fatalf("bufferedBytes = %d, want the whole file %d", c.bufferedBytes, fileSize)
	}
}

func TestPrefetchBufferedBytesStopsAtTorrentEnd(t *testing.T) {
	const pieceLength = int64(100)
	c := newTestCoordinator(cache.New(1 << 20))
	// A file that ends before the torrent does: piece 1 is shared with the next
	// file, so the window must not count past this file's length.
	c.hasAnchor = true
	c.anchor = prefetchAnchor{
		fileStart:   0,
		fileSize:    150,
		pieceLength: pieceLength,
		torrentSize: 400,
		cursor:      0,
	}
	c.cache.Put(c.key(0), make([]byte, pieceLength))
	c.cache.Put(c.key(1), make([]byte, pieceLength))
	c.cache.Put(c.key(2), make([]byte, pieceLength))
	c.updateBufferedLocked()
	if want := int64(150); c.bufferedBytes != want {
		t.Fatalf("bufferedBytes = %d, want %d clamped to the file end", c.bufferedBytes, want)
	}
}

func TestPrefetchWatermarksShrinkWithFileAndBudget(t *testing.T) {
	const (
		miB = int64(1) << 20
	)
	cases := []struct {
		name               string
		bytesToEOF         int64
		reservedPrefix     int64
		reservationBlocked bool
		wantHigh           int64
		wantLow            int64
	}{
		{
			name:       "large file keeps the defaults",
			bytesToEOF: 512 << 20,
			wantHigh:   defaultPrefetchHigh,
			wantLow:    defaultPrefetchLow,
		},
		{
			name:       "short tail shrinks the window and keeps 1:2 hysteresis",
			bytesToEOF: 40 * miB,
			wantHigh:   40 * miB,
			wantLow:    20 * miB,
		},
		{
			name:               "tight pin budget clamps to the admitted prefix",
			bytesToEOF:         512 << 20,
			reservedPrefix:     8 * miB,
			reservationBlocked: true,
			wantHigh:           8 * miB,
			wantLow:            4 * miB,
		},
		{
			name:               "exhausted budget collapses the window",
			bytesToEOF:         512 << 20,
			reservationBlocked: true,
			wantHigh:           0,
			wantLow:            0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCoordinator(cache.New(1 << 30))
			c.hasAnchor = true
			c.anchor = prefetchAnchor{fileSize: tc.bytesToEOF, pieceLength: 256 << 10, torrentSize: tc.bytesToEOF}
			c.computeWatermarksLocked(tc.reservedPrefix, tc.reservationBlocked)
			if c.effectiveHigh != tc.wantHigh {
				t.Errorf("effectiveHigh = %d, want %d", c.effectiveHigh, tc.wantHigh)
			}
			if c.effectiveLow != tc.wantLow {
				t.Errorf("effectiveLow = %d, want %d", c.effectiveLow, tc.wantLow)
			}
		})
	}
}

func TestPrefetchRefcountsShareOnePinPerPiece(t *testing.T) {
	const pieceLength = int64(64)
	store := cache.New(1 << 20)
	c := newTestCoordinator(store)
	c.hasAnchor = true
	c.anchor = prefetchAnchor{fileSize: 4 * pieceLength, pieceLength: pieceLength, torrentSize: 4 * pieceLength}

	if !c.retainLocked(0) || !c.retainLocked(0) {
		t.Fatal("two retains of one piece must both succeed")
	}
	if got := len(c.refs); got != 1 {
		t.Fatalf("refs = %d entries after two retains of one piece, want 1 (one pin)", got)
	}
	if want := pieceLength; store.PinnedBytes() != want {
		t.Fatalf("PinnedBytes = %d, want %d for a single shared pin", store.PinnedBytes(), want)
	}
	c.releaseLocked(0)
	if store.PinnedBytes() != pieceLength {
		t.Fatalf("PinnedBytes = %d after one of two releases, want the pin still held", store.PinnedBytes())
	}
	c.releaseLocked(0)
	if store.PinnedBytes() != 0 {
		t.Fatalf("PinnedBytes = %d after the last release, want 0", store.PinnedBytes())
	}
	// An extra release must not underflow into a negative pin.
	c.releaseLocked(0)
	if store.PinnedBytes() != 0 {
		t.Fatalf("PinnedBytes = %d after an extra release, want 0", store.PinnedBytes())
	}
}

func TestPrefetchCancelPiecesOnlyClearsOwnNormalLease(t *testing.T) {
	c := newTestCoordinator(cache.New(1 << 20))
	if c.priorityCancels != 0 || len(c.active) != 0 {
		t.Fatal("a fresh coordinator must hold no leases")
	}
	// cancelLeaseLocked on an index that is not active is a no-op: it must not
	// touch the torrent or the budget.
	c.cancelLeaseLocked(7)
	if c.priorityCancels != 0 || c.cancelled != 0 {
		t.Fatalf("cancelling an inactive piece changed counters: adds=%d cancelled=%d", c.priorityCancels, c.cancelled)
	}
}

func TestPrefetchUniquePieceIndexesPreservesOrder(t *testing.T) {
	got := uniquePieceIndexes([]int{3, 3, 4, 3, 5, 4})
	want := []int{3, 4, 5}
	if len(got) != len(want) {
		t.Fatalf("uniquePieceIndexes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("uniquePieceIndexes = %v, want %v", got, want)
		}
	}
}

func TestPrefetchClampCursor(t *testing.T) {
	if got := clampCursor(-5, 100); got != 0 {
		t.Fatalf("clampCursor(-5, 100) = %d, want 0", got)
	}
	if got := clampCursor(150, 100); got != 100 {
		t.Fatalf("clampCursor(150, 100) = %d, want 100", got)
	}
	if got := clampCursor(42, 100); got != 42 {
		t.Fatalf("clampCursor(42, 100) = %d, want 42", got)
	}
}

// TestPrefetchCoordinatorCloseReleasesEverything exercises the lifecycle half
// of the contract on a torrent-less coordinator: close must zero every pin and
// be idempotent.
func TestPrefetchCoordinatorCloseReleasesEverything(t *testing.T) {
	store := cache.New(1 << 20)
	c := newTestCoordinator(store)
	c.hasAnchor = true
	c.anchor = prefetchAnchor{fileSize: 4 << 10, pieceLength: 1 << 10, torrentSize: 4 << 10}
	if !c.retainLocked(0) || !c.retainLocked(1) {
		t.Fatal("test pins were refused")
	}
	c.cleanup()
	if store.PinnedBytes() != 0 {
		t.Fatalf("PinnedBytes = %d after cleanup, want 0", store.PinnedBytes())
	}
	if len(c.refs) != 0 {
		t.Fatalf("refs = %v after cleanup, want empty", c.refs)
	}
	c.cleanup()
	if store.PinnedBytes() != 0 {
		t.Fatalf("PinnedBytes = %d after a second cleanup, want 0", store.PinnedBytes())
	}
}

// TestPrefetchForegroundTicketIsIdempotent covers the deferred-finish guard: a
// read path that reports an error and then falls through must not release the
// ticket twice.
func TestPrefetchForegroundTicketIsIdempotent(t *testing.T) {
	store := cache.New(1 << 20)
	c := newTestCoordinator(store)
	c.hasAnchor = true
	c.anchor = prefetchAnchor{fileSize: 1 << 10, pieceLength: 1 << 10, torrentSize: 1 << 10}
	if !c.retainLocked(0) {
		t.Fatal("test pin was refused")
	}
	c.nextTicket = 1
	c.anchorTicket = 1
	ticket := &foregroundTicket{
		coordinator: c,
		id:          1,
		generation:  0,
		wanted:      []int{0},
		pinned:      []int{0},
		active:      map[int]int{0: 1},
	}
	c.foreground[1] = ticket
	c.foregroundPieces[0] = 1
	ticket.finish(0, 8, nil)
	if store.PinnedBytes() != 0 {
		t.Fatalf("PinnedBytes = %d after finish, want 0", store.PinnedBytes())
	}
	ticket.finish(0, 8, nil)
	if store.PinnedBytes() != 0 {
		t.Fatalf("PinnedBytes = %d after a duplicate finish, want 0", store.PinnedBytes())
	}
	if got := c.anchor.cursor; got != 8 {
		t.Fatalf("cursor = %d after a successful finish, want 8", got)
	}
}

// TestPrefetchForegroundEOFCommitsProgress treats a positive short read at the
// file end as successful cursor progress, matching io.ReaderAt callers.
func TestPrefetchForegroundEOFCommitsProgress(t *testing.T) {
	c := newTestCoordinator(cache.New(1 << 20))
	c.hasAnchor = true
	c.anchor = prefetchAnchor{fileSize: 100, pieceLength: 32, torrentSize: 100}
	c.generation = 3
	c.anchorTicket = 1

	ticket := &foregroundTicket{coordinator: c, id: 1, generation: 3}
	c.foreground[1] = ticket
	ticket.finish(90, 10, io.EOF)
	if c.anchor.cursor != 100 {
		t.Fatalf("cursor = %d after a positive EOF read, want 100", c.anchor.cursor)
	}
}

// TestPrefetchForegroundCompletionOrderDoesNotRewindCursor makes a newer
// same-generation foreground request the only completion allowed to advance
// the shared anchor.
func TestPrefetchForegroundCompletionOrderDoesNotRewindCursor(t *testing.T) {
	c := newTestCoordinator(cache.New(1 << 20))
	c.hasAnchor = true
	c.anchor = prefetchAnchor{fileSize: 1024, pieceLength: 32, torrentSize: 1024, cursor: 10}
	c.generation = 4
	c.anchorTicket = 2

	newer := &foregroundTicket{coordinator: c, id: 2, generation: 4}
	older := &foregroundTicket{coordinator: c, id: 1, generation: 4}
	c.foreground[1] = older
	c.foreground[2] = newer
	newer.finish(200, 20, nil)
	older.finish(10, 10, nil)
	if c.anchor.cursor != 220 {
		t.Fatalf("cursor = %d after out-of-order completion, want 220", c.anchor.cursor)
	}
}

// TestPrefetchStaleTicketDoesNotMoveCursor guards the generation check on
// finish: a ticket from before a jump must not drag the anchor back.
func TestPrefetchStaleTicketDoesNotMoveCursor(t *testing.T) {
	store := cache.New(1 << 20)
	c := newTestCoordinator(store)
	c.hasAnchor = true
	c.anchor = prefetchAnchor{fileSize: 1 << 20, pieceLength: 1 << 10, torrentSize: 1 << 20}
	c.generation = 5
	c.nextTicket = 1
	stale := &foregroundTicket{coordinator: c, id: 1, generation: 4}
	c.foreground[1] = stale
	stale.finish(0, 4096, nil)
	if c.anchor.cursor != 0 {
		t.Fatalf("cursor = %d after a stale ticket finished, want 0", c.anchor.cursor)
	}
}

// ensure the coordinator compiles against the context it is given.
var _ = context.Background
