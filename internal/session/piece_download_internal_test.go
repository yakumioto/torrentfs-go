package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// failingPieceSource is a pieceSource whose ReadAtContext always fails
// without touching the network. A genuine, non-cancellation BitTorrent fetch
// error is rare enough in a real swarm that it cannot be produced reliably
// from outside the package, so this fault-injection seam mirrors the one
// cache_internal_test.go and playback_window_test.go already use (a custom
// pieceSource installed directly on the Torrent) to exercise the failed
// terminal state deterministically.
type failingPieceSource struct {
	calls atomic.Int32
	err   error
}

func (f *failingPieceSource) ReadAtContext(context.Context, []byte, int64, int64) (int, error) {
	f.calls.Add(1)
	return 0, f.err
}

func (f *failingPieceSource) Close() error { return nil }

// controlledPieceSource is a pieceSource whose per-piece behaviour is
// scripted, so a test can make one piece fail while the rest block long
// enough for their siblings to be dispatched first. It also records the peak
// number of concurrent calls, which is what the concurrency budget must cap.
type controlledPieceSource struct {
	calls        atomic.Int32
	mu           sync.Mutex
	peak         int
	inFlight     int
	failIndex    int
	failErr      error
	sleepOnOther time.Duration
}

func (c *controlledPieceSource) ReadAtContext(ctx context.Context, p []byte, off, readahead int64) (int, error) {
	c.calls.Add(1)
	c.mu.Lock()
	c.inFlight++
	if c.inFlight > c.peak {
		c.peak = c.inFlight
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.inFlight--
		c.mu.Unlock()
	}()

	index := int(off / internalTestPieceLength)
	if index == c.failIndex {
		// Give the dispatcher time to fill the budget with the pieces that
		// follow before this failure lands.
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
		}
		return 0, c.failErr
	}
	if c.sleepOnOther > 0 {
		select {
		case <-time.After(c.sleepOnOther):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	copy(p, bytes.Repeat([]byte{byte(index)}, len(p)))
	return len(p), nil
}

func (c *controlledPieceSource) Close() error { return nil }

func (c *controlledPieceSource) peakConcurrency() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peak
}

// TestPieceDownloadFailsOnFetchError covers the failure path: the first
// non-cancellation fetch error moves the download to failed, records the
// error, logs it, and stops fetching the remaining pieces instead of trying
// them anyway. The connection-allowance fallback on this exit path is the
// same defer-based cleanup TestPieceDownloadExpiresWhenTimeoutElapsedWithNoPeer
// already proves for the expired path in a full real-swarm test, so it is
// not repeated here.
func TestPieceDownloadFailsOnFetchError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	content := bytes.Repeat([]byte{9}, 2*internalTestPieceLength)
	torrentPath, hash := buildInternalTestTorrent(t, dataDir, work, content)

	var logs bytes.Buffer
	sess, err := New(internalTestConfig(), internalTestTorrentDir(t, dataDir), WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if err := sess.AddTorrent(ctx, Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}

	source := &failingPieceSource{err: errors.New("simulated fetch failure")}
	st := sess.torrents[hash]
	st.mu.Lock()
	st.loader = source
	st.mu.Unlock()

	status, err := sess.StartPieceDownload(ctx, hash.HexString(), []PieceRange{{Start: 0, End: 2}}, 0)
	if err != nil {
		t.Fatalf("StartPieceDownload: %v", err)
	}
	if status.State != PieceDownloadRunning {
		t.Fatalf("initial state = %q, want running", status.State)
	}

	deadline := time.Now().Add(10 * time.Second)
	var final PieceDownloadStatus
	for time.Now().Before(deadline) {
		final, err = sess.PieceDownloadStatus(hash.HexString())
		if err != nil {
			t.Fatalf("PieceDownloadStatus: %v", err)
		}
		if final.State == PieceDownloadFailed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if final.State != PieceDownloadFailed {
		t.Fatalf("state = %q, want failed", final.State)
	}
	if final.Error == "" {
		t.Fatal("error is empty for a failed download")
	}
	if final.FinishedAt.IsZero() {
		t.Fatal("finished_at is zero for a failed download")
	}
	if final.DonePieces != 0 {
		t.Fatalf("done_pieces = %d, want 0 (the only attempted piece failed)", final.DonePieces)
	}
	// Pieces are dispatched concurrently, so a piece that was already handed
	// to a worker when the failure lands may also have attempted its fetch;
	// what the failure must stop is the pieces still queued for a budget
	// slot, not the one already in flight next to it.
	if got := source.calls.Load(); got < 1 || got > 2 {
		t.Fatalf("fetch attempts = %d, want between 1 and 2 (the download must stop instead of retrying or continuing)", got)
	}
	if !strings.Contains(logs.String(), "piece download failed") {
		t.Fatalf("log output = %q, want it to record the failure", logs.String())
	}
}

// TestPieceDownloadCapsConcurrencyAcrossBudget covers the budget itself: a
// download requesting more pieces than pieceDownloadConcurrency must fetch
// every piece (the last status is completed) while never having more than the
// budget's worth of fetches in flight at once. The source sleeps on every
// piece, so absent a budget the peak would be the full request size.
func TestPieceDownloadCapsConcurrencyAcrossBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	const pieces = pieceDownloadConcurrency + 8
	content := bytes.Repeat([]byte{5}, pieces*internalTestPieceLength)
	torrentPath, hash := buildInternalTestTorrent(t, dataDir, work, content)

	sess, err := New(internalTestConfig(), internalTestTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if err := sess.AddTorrent(ctx, Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}

	source := &controlledPieceSource{failIndex: -1, sleepOnOther: 20 * time.Millisecond}
	st := sess.torrents[hash]
	st.mu.Lock()
	st.loader = source
	st.mu.Unlock()

	if _, err := sess.StartPieceDownload(ctx, hash.HexString(), []PieceRange{{Start: 0, End: pieces}}, 0); err != nil {
		t.Fatalf("StartPieceDownload: %v", err)
	}
	if err := waitForStatus(ctx, sess, hash.HexString(), PieceDownloadCompleted); err != nil {
		t.Fatal(err)
	}
	final, err := sess.PieceDownloadStatus(hash.HexString())
	if err != nil {
		t.Fatalf("PieceDownloadStatus: %v", err)
	}
	if final.DonePieces != pieces {
		t.Fatalf("done_pieces = %d, want all %d", final.DonePieces, pieces)
	}
	if peak := source.peakConcurrency(); peak > pieceDownloadConcurrency {
		t.Fatalf("peak concurrent fetches = %d, want at most %d", peak, pieceDownloadConcurrency)
	} else if peak < 2 {
		t.Fatalf("peak concurrent fetches = %d, want the download to fetch pieces concurrently", peak)
	}
}

// TestPieceDownloadFailureStopsPiecesQueuedForTheBudget covers the failure
// path under concurrency: a request larger than the budget that fails on one
// in-flight piece must not go on to start the pieces still waiting for a slot.
// The failing piece waits long enough for the dispatcher to fill every slot,
// so exactly pieceDownloadConcurrency fetches are attempted and the rest are
// dropped unresolved.
func TestPieceDownloadFailureStopsPiecesQueuedForTheBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	const pieces = pieceDownloadConcurrency + 4
	content := bytes.Repeat([]byte{6}, pieces*internalTestPieceLength)
	torrentPath, hash := buildInternalTestTorrent(t, dataDir, work, content)

	sess, err := New(internalTestConfig(), internalTestTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if err := sess.AddTorrent(ctx, Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}

	source := &controlledPieceSource{failIndex: 0, failErr: errors.New("simulated fetch failure"), sleepOnOther: 10 * time.Second}
	st := sess.torrents[hash]
	st.mu.Lock()
	st.loader = source
	st.mu.Unlock()

	if _, err := sess.StartPieceDownload(ctx, hash.HexString(), []PieceRange{{Start: 0, End: pieces}}, 0); err != nil {
		t.Fatalf("StartPieceDownload: %v", err)
	}
	if err := waitForStatus(ctx, sess, hash.HexString(), PieceDownloadFailed); err != nil {
		t.Fatal(err)
	}
	if got := source.calls.Load(); got != pieceDownloadConcurrency {
		t.Fatalf("fetch attempts = %d, want exactly %d (the pieces queued for a slot must not be started)", got, pieceDownloadConcurrency)
	}

	// A failed download must hand back every slot it held, so a later
	// download on the same torrent is not stuck waiting for them.
	fast := &controlledPieceSource{failIndex: -1}
	st.mu.Lock()
	st.loader = fast
	st.mu.Unlock()
	if _, err := sess.StartPieceDownload(ctx, hash.HexString(), []PieceRange{{Start: 0, End: pieces}}, 0); err != nil {
		t.Fatalf("second StartPieceDownload: %v", err)
	}
	if err := waitForStatus(ctx, sess, hash.HexString(), PieceDownloadCompleted); err != nil {
		t.Fatalf("failed download did not return its budget slots: %v", err)
	}
}

// waitForStatus polls the session's piece download for want, failing with the
// last status if the deadline passes first.
func waitForStatus(ctx context.Context, sess *Session, id string, want PieceDownloadState) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	var last PieceDownloadStatus
	for {
		status, err := sess.PieceDownloadStatus(id)
		if err != nil {
			return err
		}
		last = status
		if status.State == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("piece download never reached %q; last status %+v", want, last)
		case <-ticker.C:
		}
	}
}

// TestPieceDownloadExpiryReturnsBudgetSlots covers the expiry path's budget
// accounting: a download that expires must hand back every slot it held, so a
// later download on the same torrent is not stuck waiting for them.
func TestPieceDownloadExpiryReturnsBudgetSlots(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	const pieces = pieceDownloadConcurrency + 1
	content := bytes.Repeat([]byte{4}, pieces*internalTestPieceLength)
	torrentPath, hash := buildInternalTestTorrent(t, dataDir, work, content)

	sess, err := New(internalTestConfig(), internalTestTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if err := sess.AddTorrent(ctx, Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}

	blocking := &controlledPieceSource{failIndex: -1, sleepOnOther: 10 * time.Second}
	st := sess.torrents[hash]
	st.mu.Lock()
	st.loader = blocking
	st.mu.Unlock()

	if _, err := sess.StartPieceDownload(ctx, hash.HexString(), []PieceRange{{Start: 0, End: pieces}}, 25*time.Millisecond); err != nil {
		t.Fatalf("StartPieceDownload: %v", err)
	}
	if err := waitForStatus(ctx, sess, hash.HexString(), PieceDownloadExpired); err != nil {
		t.Fatal(err)
	}

	fast := &controlledPieceSource{failIndex: -1}
	st.mu.Lock()
	st.loader = fast
	st.mu.Unlock()
	if _, err := sess.StartPieceDownload(ctx, hash.HexString(), []PieceRange{{Start: 0, End: pieces}}, 0); err != nil {
		t.Fatalf("second StartPieceDownload: %v", err)
	}
	if err := waitForStatus(ctx, sess, hash.HexString(), PieceDownloadCompleted); err != nil {
		t.Fatalf("expired download did not return its budget slots: %v", err)
	}
	final, err := sess.PieceDownloadStatus(hash.HexString())
	if err != nil {
		t.Fatalf("PieceDownloadStatus: %v", err)
	}
	if final.DonePieces != pieces {
		t.Fatalf("done_pieces = %d, want all %d", final.DonePieces, pieces)
	}
}
