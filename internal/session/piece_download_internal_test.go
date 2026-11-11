package session

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
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
	if got := source.calls.Load(); got != 1 {
		t.Fatalf("fetch attempts = %d, want exactly 1 (the loop must stop at the first error instead of trying the remaining piece)", got)
	}
	if !strings.Contains(logs.String(), "piece download failed") {
		t.Fatalf("log output = %q, want it to record the failure", logs.String())
	}
}
