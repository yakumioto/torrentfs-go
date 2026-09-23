package session_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"golang.org/x/time/rate"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

// prefetchFile wraps one opened session handle so a test can drive reads at
// torrent-global piece offsets through the real FUSE-facing reader.
type prefetchFile struct {
	ra interface {
		ReadAt([]byte, int64) (int, error)
	}
	closer interface{ Close() error }
}

func openPrefetchFile(t *testing.T, sess *session.Session, hash metainfo.Hash, path string) (*prefetchFile, *session.Torrent) {
	t.Helper()
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatalf("torrent %s is not registered", hash)
	}
	ra, err := sess.OpenFile(hash, path)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", path, err)
	}
	closer, ok := ra.(interface{ Close() error })
	if !ok {
		t.Fatal("OpenFile did not return a closable handle")
	}
	return &prefetchFile{ra: ra, closer: closer}, st
}

func (f *prefetchFile) read(t *testing.T, torrentOffset int64, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := f.ra.ReadAt(buf, torrentOffset); err != nil {
		t.Fatalf("ReadAt(%d): %v", torrentOffset, err)
	}
	return buf
}

func (f *prefetchFile) close() {
	if f.closer != nil {
		_ = f.closer.Close()
	}
}

// waitPrefetchState polls the torrent's coordinator until want holds.
func waitPrefetchState(t *testing.T, ctx context.Context, st *session.Torrent, what string, want func(session.PrefetchSnapshot) bool) session.PrefetchSnapshot {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last session.PrefetchSnapshot
	for time.Now().Before(deadline) {
		last = session.PrefetchSnapshotForTest(st)
		if want(last) {
			return last
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v (last state %+v)", what, ctx.Err(), last)
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for %s; last state %+v", what, last)
	return last
}

func defaultTestPrefetchPieces() int { return session.PrefetchDefaultPiecesForTest() }

// seedPrefetchTorrent registers a peerless single-file torrent of the given
// piece count and seeds just the given pieces, so a read of a seeded piece is a
// cache hit and the window ahead of it stays empty.
func seedPrefetchTorrent(t *testing.T, sess *session.Session, name string, pieces int, seeded ...int) (metainfo.Hash, []byte) {
	t.Helper()
	content := make([]byte, pieces*testPieceLength)
	for i := range content {
		content[i] = byte(i*11 + i/97 + 1)
	}
	torrentBytes, hash := buildSingleFileTorrentBytes(t, name, content, nil)
	if err := sess.AddTorrent(context.Background(), session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("AddTorrent(%s): %v", name, err)
	}
	for _, index := range seeded {
		if err := sess.SeedPieceForTest(hash, index, content); err != nil {
			t.Fatalf("SeedPieceForTest(%s, %d): %v", name, index, err)
		}
	}
	return hash, content
}

// TestPrefetchActivatesWindowUpToGlobalBudget proves the coordinator activates
// the head of the forward window, caps the claim at the session-wide budget,
// and does not re-request a piece it already holds.
func TestPrefetchActivatesWindowUpToGlobalBudget(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	sess := newLoopbackSession(t, testConfig(), filepath.Join(work, "data"))
	hash, _ := seedPrefetchTorrent(t, sess, "payload.bin", 16, 0)
	opened, st := openPrefetchFile(t, sess, hash, "payload.bin")
	defer opened.close()

	// The anchor read is a cache hit; the 15 pieces ahead are missing, so the
	// coordinator may prefetch at most its budget of them.
	opened.read(t, 0, 64)
	snapshot := waitPrefetchState(t, ctx, st, "the budget-sized active set", func(s session.PrefetchSnapshot) bool {
		return s.ActivePieces == defaultTestPrefetchPieces()
	})
	if snapshot.MaxActivePieces > defaultTestPrefetchPieces() {
		t.Fatalf("max active pieces = %d, want at most %d", snapshot.MaxActivePieces, defaultTestPrefetchPieces())
	}
	if _, used := session.PrefetchBudgetUsageForTest(sess); used != defaultTestPrefetchPieces() {
		t.Fatalf("session background budget used = %d, want %d", used, defaultTestPrefetchPieces())
	}
	if snapshot.DedupeCount != 0 {
		t.Fatalf("a single cold read produced %d duplicate piece activations", snapshot.DedupeCount)
	}

	// A second read inside the same window must not claim another lease: every
	// piece it wants is either already resident or already held.
	opened.read(t, int64(testPieceLength/2), 64)
	after := session.PrefetchSnapshotForTest(st)
	if after.PriorityAdds != snapshot.PriorityAdds {
		t.Fatalf("priority adds went %d -> %d for an overlapping read; the window must stay deduped",
			snapshot.PriorityAdds, after.PriorityAdds)
	}
	if after.ActivePieces > defaultTestPrefetchPieces() {
		t.Fatalf("active pieces = %d after an overlapping read, want at most %d", after.ActivePieces, defaultTestPrefetchPieces())
	}
}

// TestPrefetchBudgetIsGlobalAcrossTorrents proves the cap counts per session,
// is returned when a handle closes, and can be reused by another torrent.
func TestPrefetchBudgetIsGlobalAcrossTorrents(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	sess := newLoopbackSession(t, testConfig(), filepath.Join(work, "data"))
	firstHash, _ := seedPrefetchTorrent(t, sess, "first.bin", 16, 0)
	secondHash, _ := seedPrefetchTorrent(t, sess, "second.bin", 16, 0)

	first, firstTorrent := openPrefetchFile(t, sess, firstHash, "first.bin")
	second, secondTorrent := openPrefetchFile(t, sess, secondHash, "second.bin")

	first.read(t, 0, 64)
	waitPrefetchState(t, ctx, firstTorrent, "the first torrent to claim the budget", func(s session.PrefetchSnapshot) bool {
		return s.ActivePieces == defaultTestPrefetchPieces()
	})

	second.read(t, 0, 64)
	blocked := waitPrefetchState(t, ctx, secondTorrent, "the second torrent to be budget blocked", func(s session.PrefetchSnapshot) bool {
		return s.BudgetBlocks > 0
	})
	if blocked.ActivePieces != 0 {
		t.Fatalf("second torrent holds %d active pieces while the global budget is exhausted, want 0", blocked.ActivePieces)
	}
	if limit, used := session.PrefetchBudgetUsageForTest(sess); used > limit {
		t.Fatalf("global background budget used = %d, want at most %d", used, limit)
	}

	// Releasing the first torrent's only handle must return its window and its
	// tokens, and the next foreground read on the second torrent must pick them
	// up without exceeding the limit.
	first.close()
	waitPrefetchState(t, ctx, firstTorrent, "the first torrent to release its window", func(s session.PrefetchSnapshot) bool {
		return s.ActivePieces == 0 && s.PinnedPieces == 0
	})
	second.read(t, 0, 64)
	recovered := waitPrefetchState(t, ctx, secondTorrent, "the second torrent to reuse the freed budget", func(s session.PrefetchSnapshot) bool {
		return s.ActivePieces > 0
	})
	if recovered.ActivePieces > defaultTestPrefetchPieces() {
		t.Fatalf("second torrent active pieces = %d, want at most %d", recovered.ActivePieces, defaultTestPrefetchPieces())
	}
	if limit, used := session.PrefetchBudgetUsageForTest(sess); used > limit {
		t.Fatalf("global background budget used = %d after reuse, want at most %d", used, limit)
	}
	second.close()
}

// TestPrefetchBackwardSeekAdvancesGenerationAndCancelsOldWork proves a cold
// seek invalidates the previous window: the generation advances, the old leases
// are cancelled, and the new position's pieces take their place.
func TestPrefetchBackwardSeekAdvancesGenerationAndCancelsOldWork(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	sess := newLoopbackSession(t, testConfig(), filepath.Join(work, "data"))
	hash, _ := seedPrefetchTorrent(t, sess, "payload.bin", 64, 0, 60)
	opened, st := openPrefetchFile(t, sess, hash, "payload.bin")
	defer opened.close()

	// Start deep in the file so the next read is a backward jump.
	opened.read(t, 60*testPieceLength, 64)
	before := waitPrefetchState(t, ctx, st, "the forward window to activate", func(s session.PrefetchSnapshot) bool {
		return s.ActivePieces > 0
	})

	opened.read(t, 0, 64)
	candidate := waitPrefetchState(t, ctx, st, "the first backward probe", func(s session.PrefetchSnapshot) bool {
		return s.Generation == before.Generation && s.CandidatePresent
	})
	if candidate.Generation != before.Generation {
		t.Fatalf("generation = %d after the first seek probe, want %d", candidate.Generation, before.Generation)
	}
	opened.read(t, 64, 64)
	after := waitPrefetchState(t, ctx, st, "the seek to establish the new window", func(s session.PrefetchSnapshot) bool {
		return s.Generation > before.Generation && s.ActivePieces > 0
	})
	if after.Cursor != 128 {
		t.Fatalf("cursor = %d after the confirmed seek read, want 128", after.Cursor)
	}

	// The active window pieces sit on the priority ladder; the piece the reader
	// left behind no longer carries a foreground demand.
	runs := session.PieceStateRunsForTest(st)
	if len(after.ActivePieceIndexes) == 0 {
		t.Fatal("the new window has no active background pieces")
	}
	if got := piecePriorityAt(runs, after.ActivePieceIndexes[0]); got < torrent.PiecePriorityNormal {
		t.Fatalf("active window piece priority = %v, want at least Normal", got)
	}
	if got := piecePriorityAt(runs, 60); got >= torrent.PiecePriorityNow {
		t.Fatalf("pre-seek piece priority = %v, want below Now", got)
	}
}

// TestPrefetchPausesWhenWindowIsResident proves the high-water mark stops
// expansion: a wholly resident window holds no lease and releases every pin
// when its last handle closes.
func TestPrefetchPausesWhenWindowIsResident(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := testTorrentDir(t, dataDir)
	content := make([]byte, 4*testPieceLength)
	for i := range content {
		content[i] = byte(i*7 + 3)
	}
	torrentPath, hash := buildSingleFileTorrent(t, work, "payload.bin", content)
	sess := newLoopbackSession(t, testConfig(), torrentsDir)
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	seedPieces(t, sess, hash, content)
	waitCached(t, ctx, st)

	opened, openedTorrent := openPrefetchFile(t, sess, hash, "payload.bin")
	opened.read(t, 0, 64)
	snapshot := waitPrefetchState(t, ctx, openedTorrent, "the window to pause", func(s session.PrefetchSnapshot) bool {
		return s.State == "paused"
	})
	// Buffered bytes count forward from the cursor. The snapshot may be taken
	// just before or just after the anchor read's completion advances the
	// cursor, so the whole file minus at most the 64 consumed bytes is right.
	if min, max := int64(len(content))-64, int64(len(content)); snapshot.BufferedBytes < min || snapshot.BufferedBytes > max {
		t.Fatalf("bufferedBytes = %d, want within [%d, %d]", snapshot.BufferedBytes, min, max)
	}
	if snapshot.ActivePieces != 0 {
		t.Fatalf("active pieces = %d with the whole window resident, want 0", snapshot.ActivePieces)
	}
	if _, used := session.PrefetchBudgetUsageForTest(sess); used != 0 {
		t.Fatalf("background budget used = %d with a fully resident window, want 0", used)
	}
	if snapshot.PinnedPieces == 0 {
		t.Fatal("the resident read window was not pinned")
	}
	opened.close()
	waitPrefetchState(t, ctx, openedTorrent, "the pins to be released on close", func(s session.PrefetchSnapshot) bool {
		return s.PinnedPieces == 0
	})
	if got := st.CachedBytes(); got != int64(len(content)) {
		t.Fatalf("closing the handle discarded resident data: cached bytes = %d, want %d", got, len(content))
	}
}

// TestPrefetchRefillsAfterTheWindowDrains proves the hysteresis: a window that
// was full enough to pause resumes filling once the buffer falls to the low
// watermark, and it resumes on the next foreground progress rather than by
// polling.
func TestPrefetchRefillsAfterTheWindowDrains(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := testTorrentDir(t, dataDir)
	const pieces = 8
	content := make([]byte, pieces*testPieceLength)
	for i := range content {
		content[i] = byte(i*13 + 5)
	}
	torrentPath, hash := buildSingleFileTorrent(t, work, "payload.bin", content)
	sess := newLoopbackSession(t, testConfig(), torrentsDir)
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	seedPieces(t, sess, hash, content)
	waitCached(t, ctx, st)

	opened, st := openPrefetchFile(t, sess, hash, "payload.bin")
	defer opened.close()
	opened.read(t, 0, 64)
	full := waitPrefetchState(t, ctx, st, "the window to pause", func(s session.PrefetchSnapshot) bool {
		return s.State == "paused"
	})
	if full.ActivePieces != 0 {
		t.Fatalf("active pieces = %d with a full window, want 0", full.ActivePieces)
	}

	// Drop everything but the piece the cursor sits in, so the verified forward
	// buffer falls below the effective low watermark.
	if err := sess.EvictPiecesForTest(hash, 0); err != nil {
		t.Fatalf("EvictPiecesForTest: %v", err)
	}
	opened.read(t, 0, 64)
	refilled := waitPrefetchState(t, ctx, st, "the window to refill", func(s session.PrefetchSnapshot) bool {
		return s.State == "filling" && s.ActivePieces > 0
	})
	if refilled.BufferedBytes >= full.BufferedBytes {
		t.Fatalf("buffered bytes = %d after the drain, want below the previous %d", refilled.BufferedBytes, full.BufferedBytes)
	}
	if refilled.ActivePieces > defaultTestPrefetchPieces() {
		t.Fatalf("active pieces = %d after refilling, want at most %d", refilled.ActivePieces, defaultTestPrefetchPieces())
	}
	if refilled.EffectiveLow > refilled.EffectiveHigh {
		t.Fatalf("effective low %d exceeds effective high %d", refilled.EffectiveLow, refilled.EffectiveHigh)
	}
}

// TestPrefetchForegroundOutranksBackgroundLease drives a real loopback swarm
// and observes the priority ladder: the piece a reader is waiting on carries
// the reader's Now demand while the coordinator's window pieces stay at the
// lower Normal level, so prefetch can never outrank the playback read.
func TestPrefetchForegroundOutranksBackgroundLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	work := t.TempDir()
	tracker := newLoopbackTracker(t)
	pieceCount := 96
	content := make([]byte, pieceCount*testPieceLength)
	for i := range content {
		content[i] = byte(i*29 + i/211 + 1)
	}
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, [][]string{{tracker.url}})
	hashHex := hash.HexString()

	seeder := newLoopbackSession(t, testConfig(), filepath.Join(work, "seeder-data"))
	if err := seeder.AddTorrent(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("seeder AddTorrent: %v", err)
	}
	seederHandle, ok := seeder.Torrent(hash)
	if !ok {
		t.Fatal("seeder did not register the torrent")
	}
	seedPieces(t, seeder, hash, content)
	waitCached(t, ctx, seederHandle)

	// A deliberately slow leecher keeps the coordinator's window pieces
	// outstanding while the reader waits, so the priority observation below is
	// made against a live background lease rather than a completed one.
	leecher := newLoopbackSessionWithCustomize(t, testConfig(), filepath.Join(work, "leecher-data"), func(cc *session.TorrentClientConfig) {
		cc.DownloadRateLimiter = rate.NewLimiter(rate.Limit(512<<10), 64<<10)
	})
	if err := leecher.AddTorrent(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("leecher AddTorrent: %v", err)
	}
	if err := waitFor(ctx, func() bool { return tracker.peerCount(hashHex) >= 2 }); err != nil {
		t.Fatalf("leecher did not connect to the seeder: %v", err)
	}
	opened, st := openPrefetchFile(t, leecher, hash, "payload.bin")
	defer opened.close()

	// Read a piece far enough into the file that the coordinator's window and
	// the reader's target are different pieces.
	targetPiece := 3
	targetOffset := int64(targetPiece) * testPieceLength
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		_, err := opened.ra.ReadAt(buf, targetOffset+128)
		if err == nil && !bytes.Equal(buf, content[targetOffset+128:targetOffset+128+64]) {
			err = errReadDataMismatch
		}
		done <- err
	}()

	targetAt, err := waitForPiecePriority(ctx, st, targetPiece, torrent.PiecePriorityNow)
	if err != nil {
		t.Fatalf("the foreground piece never reached Now priority: %v", err)
	}
	// While the reader waits, at least one other piece in the window must be
	// held by the coordinator below the reader's priority level.
	windowAt, err := waitForWindowPriorityBelow(ctx, st, targetPiece, torrent.PiecePriorityNormal, torrent.PiecePriorityNow)
	if err != nil {
		t.Fatalf("no background window piece below Now while the read waited: %v", err)
	}
	t.Logf("target=Now at %s, background=Normal from %s", targetAt, windowAt)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("foreground read: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("foreground read never completed: %v", ctx.Err())
	}
	snapshot := session.PrefetchSnapshotForTest(st)
	if snapshot.MaxActivePieces > defaultTestPrefetchPieces() {
		t.Fatalf("max active background pieces = %d, want at most %d", snapshot.MaxActivePieces, defaultTestPrefetchPieces())
	}
}

var errReadDataMismatch = errors.New("foreground read data mismatch")

// waitForWindowPriorityBelow waits until some piece in the forward window holds
// a priority in [min, max).
func waitForWindowPriorityBelow(ctx context.Context, st *session.Torrent, target int, min, max torrent.PiecePriority) (time.Time, error) {
	for {
		runs := session.PieceStateRunsForTest(st)
		for piece := target + 1; piece < target+64; piece++ {
			if priority := piecePriorityAt(runs, piece); priority >= min && priority < max {
				return time.Now(), nil
			}
		}
		select {
		case <-ctx.Done():
			return time.Time{}, ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
}
