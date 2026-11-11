package session_test

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

// TestPieceDownloadFetchesOnlyRequestedPiecesAndGrantsReadingAllowance is the
// end-to-end proof of the piece download seam: a leecher with no open file
// handle fetches exactly the requested pieces from a real peer, holds the
// reading connection allowance while the download is in flight, and falls back
// once it is done and the read grace window has elapsed.
func TestPieceDownloadFetchesOnlyRequestedPiecesAndGrantsReadingAllowance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	work := t.TempDir()
	content := bytes.Repeat([]byte{3}, 3*testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	hashHex := hash.HexString()

	seeder := seederFor(t, ctx, filepath.Join(work, "seeder"), torrentBytes, hash, content)
	seederAddr := torrent.PeerInfo{
		Addr: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(seeder.EffectiveListenPort())),
	}

	cfg := testConfig()
	leecher := newLoopbackSessionWithCustomize(t, cfg, filepath.Join(work, "leecher"),
		func(cc *session.TorrentClientConfig) { cc.DropMutuallyCompletePeers = false })
	_, advance := fakeConnectionClock(t, leecher)
	if err := leecher.AddTorrent(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("leecher AddTorrent: %v", err)
	}
	state, ok := leecher.Torrent(hash)
	if !ok {
		t.Fatal("leecher did not register the torrent")
	}
	session.UnderlyingTorrentForTest(state).AddPeers([]torrent.PeerInfo{seederAddr})

	status, err := leecher.StartPieceDownload(ctx, hashHex, []session.PieceRange{{Start: 0, End: 1}})
	if err != nil {
		t.Fatalf("StartPieceDownload: %v", err)
	}
	if status.State != session.PieceDownloadRunning || status.RequestedPieces != 1 || status.DonePieces != 0 {
		t.Fatalf("initial status = %+v, want running, 1 requested, 0 done", status)
	}

	// The download is in flight: no file handle is open, yet the torrent must
	// hold the reading allowance so it can reach the seeder.
	session.AllocateConnectionsForTest(leecher)
	if got := session.ConnectionStateForTest(state); got != "reading" {
		t.Fatalf("a torrent with a piece download in flight is in state %q, want reading", got)
	}
	if got := session.ConnectionAllowanceForTest(state); got != cfg.Connections.ReadingConns {
		t.Fatalf("a torrent with a piece download in flight has allowance %d, want %d", got, cfg.Connections.ReadingConns)
	}

	if err := waitFor(ctx, func() bool {
		s, err := leecher.PieceDownloadStatus(hashHex)
		return err == nil && s.State == session.PieceDownloadCompleted
	}); err != nil {
		t.Fatalf("piece download never completed: %v", err)
	}
	final, err := leecher.PieceDownloadStatus(hashHex)
	if err != nil {
		t.Fatalf("PieceDownloadStatus: %v", err)
	}
	if final.DonePieces != 1 || final.FinishedAt.IsZero() {
		t.Fatalf("final status = %+v, want 1 done piece and a finished_at", final)
	}

	statusView, err := leecher.TorrentStatusFor(hashHex)
	if err != nil {
		t.Fatalf("TorrentStatusFor: %v", err)
	}
	if len(statusView.Pieces) != 3 {
		t.Fatalf("piece count = %d, want 3", len(statusView.Pieces))
	}
	if !statusView.Pieces[0].Cached {
		t.Fatal("requested piece 0 is not cached")
	}
	if statusView.Pieces[1].Cached || statusView.Pieces[2].Cached {
		t.Fatalf("pieces outside the requested range are cached: %+v", statusView.Pieces)
	}
	if leecher.RuntimeStats().DownloadedBytes <= 0 {
		t.Fatal("DownloadedBytes did not grow from the piece download")
	}

	// Once finished the torrent still holds the allowance (the finish records
	// a read), then drops to the probe allowance after the grace window.
	session.AllocateConnectionsForTest(leecher)
	if got := session.ConnectionStateForTest(state); got != "reading" {
		t.Fatalf("a torrent right after its piece download finished is in state %q, want reading", got)
	}
	advance(time.Duration(cfg.Connections.ReadGrace) + time.Second)
	session.AllocateConnectionsForTest(leecher)
	if got := session.ConnectionStateForTest(state); got != "probe" {
		t.Fatalf("a torrent past the read grace after its piece download is in state %q, want probe", got)
	}
	if got := session.ConnectionAllowanceForTest(state); got != 1 {
		t.Fatalf("a torrent past the read grace after its piece download has allowance %d, want the probe allowance 1", got)
	}
}

// TestPieceDownloadAlreadyCachedPiecesCompleteImmediately covers the
// one-shot rule: a piece already resident in the cache counts as done without
// a fetch, and a request that is fully satisfied by the cache completes
// synchronously.
func TestPieceDownloadAlreadyCachedPiecesCompleteImmediately(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	work := t.TempDir()
	content := bytes.Repeat([]byte{5}, 3*testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	hashHex := hash.HexString()

	seeder := seederFor(t, ctx, filepath.Join(work, "seeder"), torrentBytes, hash, content)

	status, err := seeder.StartPieceDownload(ctx, hashHex, []session.PieceRange{{Start: 0, End: 2}})
	if err != nil {
		t.Fatalf("StartPieceDownload: %v", err)
	}
	if status.State != session.PieceDownloadCompleted {
		t.Fatalf("state = %q, want completed immediately", status.State)
	}
	if status.RequestedPieces != 2 || status.DonePieces != 2 {
		t.Fatalf("status = %+v, want 2 requested and 2 done", status)
	}
	if status.FinishedAt.IsZero() {
		t.Fatal("finished_at is zero for an immediately completed download")
	}
}

// TestPieceDownloadMergesOverlappingRanges covers deduplication: overlapping
// and repeated ranges count each piece once.
func TestPieceDownloadMergesOverlappingRanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	work := t.TempDir()
	content := bytes.Repeat([]byte{6}, 3*testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	hashHex := hash.HexString()

	seeder := seederFor(t, ctx, filepath.Join(work, "seeder"), torrentBytes, hash, content)

	status, err := seeder.StartPieceDownload(ctx, hashHex, []session.PieceRange{
		{Start: 0, End: 2},
		{Start: 1, End: 3},
	})
	if err != nil {
		t.Fatalf("StartPieceDownload: %v", err)
	}
	if status.RequestedPieces != 3 {
		t.Fatalf("requested_pieces = %d, want 3 after deduplication", status.RequestedPieces)
	}
	if status.DonePieces != 3 || status.State != session.PieceDownloadCompleted {
		t.Fatalf("status = %+v, want 3 done and completed", status)
	}
}

// TestPieceDownloadRejectsInvalidRanges covers the request-validation rules: a
// range must be non-empty, half-open, in bounds, and the list itself must not
// be empty.
func TestPieceDownloadRejectsInvalidRanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	work := t.TempDir()
	content := bytes.Repeat([]byte{8}, 3*testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	hashHex := hash.HexString()

	seeder := seederFor(t, ctx, filepath.Join(work, "seeder"), torrentBytes, hash, content)

	tests := []struct {
		name   string
		ranges []session.PieceRange
	}{
		{"empty list", nil},
		{"negative start", []session.PieceRange{{Start: -1, End: 1}}},
		{"end equal to start", []session.PieceRange{{Start: 1, End: 1}}},
		{"end before start", []session.PieceRange{{Start: 2, End: 1}}},
		{"end past piece count", []session.PieceRange{{Start: 0, End: 4}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := seeder.StartPieceDownload(ctx, hashHex, tt.ranges)
			if !errors.Is(err, session.ErrInvalidPieceDownloadRequest) {
				t.Fatalf("StartPieceDownload(%v) error = %v, want ErrInvalidPieceDownloadRequest", tt.ranges, err)
			}
		})
	}
}

// TestPieceDownloadUnknownTorrent covers both the start and the status paths
// for an id with no registered torrent or deletion.
func TestPieceDownloadUnknownTorrent(t *testing.T) {
	ctx := testTimeout(t)
	sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))

	bogus := strings.Repeat("ab", 20)
	if _, err := sess.StartPieceDownload(ctx, bogus, []session.PieceRange{{Start: 0, End: 1}}); !errors.Is(err, session.ErrUnknownTorrent) {
		t.Fatalf("StartPieceDownload unknown = %v, want ErrUnknownTorrent", err)
	}
	if _, err := sess.PieceDownloadStatus(bogus); !errors.Is(err, session.ErrUnknownTorrent) {
		t.Fatalf("PieceDownloadStatus unknown = %v, want ErrUnknownTorrent", err)
	}
}

// TestPieceDownloadRejectedWhileDeletionHoldsTorrent mirrors
// TestSetFavoriteRejectedWhileDeletionHoldsTorrent: a torrent the deletion path
// owns cannot be handed a new piece download. StateDeleting and
// StateDeleteFailed share one guard, so the fixture uses the state that
// reloads without a matching .torrent file; the cleanup hook keeps startup
// reconciliation from resolving the failure out from under the test, exactly
// as it does for the favorite fixture.
func TestPieceDownloadRejectedWhileDeletionHoldsTorrent(t *testing.T) {
	ctx := testTimeout(t)
	torrentsDir := testTorrentDir(t, filepath.Join(t.TempDir(), "data"))
	hash := favoriteRegistryHash(0x03)
	writeFavoriteRegistry(t, torrentsDir, hash, string(session.StateDeleteFailed), time.Now().UTC(), false)
	restore := session.SetSubtitleCleanupHook(func(metainfo.Hash) error { return errors.New("cleanup still failing") })
	defer restore()

	sess := newManageSession(t, torrentsDir)
	if _, err := sess.StartPieceDownload(ctx, hash.HexString(), []session.PieceRange{{Start: 0, End: 1}}); !errors.Is(err, session.ErrDeleting) {
		t.Fatalf("StartPieceDownload while deletion holds the torrent = %v, want ErrDeleting", err)
	}
}

// TestPieceDownloadRejectedWhileAdding covers a magnet whose metadata has not
// arrived yet: it is known but not ready.
func TestPieceDownloadRejectedWhileAdding(t *testing.T) {
	ctx := testTimeout(t)
	sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))

	added, err := sess.AddTorrentAndPersist(ctx, session.Source{MagnetURI: "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20)})
	if err != nil {
		t.Fatalf("AddTorrentAndPersist: %v", err)
	}
	if added.State != session.StateAdding {
		t.Fatalf("state = %s, want adding", added.State)
	}
	if _, err := sess.StartPieceDownload(ctx, added.ID, []session.PieceRange{{Start: 0, End: 1}}); !errors.Is(err, session.ErrTorrentNotReady) {
		t.Fatalf("StartPieceDownload while adding = %v, want ErrTorrentNotReady", err)
	}
}

// TestCancelPieceDownloadStopsInFlightDownload covers the cancel path: a
// download with no reachable peer hangs until explicitly cancelled, and
// cancelling resolves it synchronously without waiting on the background
// fetch loop to notice.
func TestCancelPieceDownloadStopsInFlightDownload(t *testing.T) {
	ctx := testTimeout(t)
	sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))
	content := bytes.Repeat([]byte{1}, 2*testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("AddTorrentAndPersist: %v", err)
	}
	hashHex := hash.HexString()

	status, err := sess.StartPieceDownload(ctx, hashHex, []session.PieceRange{{Start: 0, End: 2}})
	if err != nil {
		t.Fatalf("StartPieceDownload: %v", err)
	}
	if status.State != session.PieceDownloadRunning {
		t.Fatalf("state = %q, want running (no peer can have served it yet)", status.State)
	}

	if err := sess.CancelPieceDownload(hashHex); err != nil {
		t.Fatalf("CancelPieceDownload: %v", err)
	}
	final, err := sess.PieceDownloadStatus(hashHex)
	if err != nil {
		t.Fatalf("PieceDownloadStatus: %v", err)
	}
	if final.State != session.PieceDownloadCancelled {
		t.Fatalf("state = %q, want cancelled", final.State)
	}
	if final.DonePieces != 0 {
		t.Fatalf("done_pieces = %d, want 0 (no peer ever served a piece)", final.DonePieces)
	}
	if final.FinishedAt.IsZero() {
		t.Fatal("finished_at is zero after cancelling")
	}
}

// TestCancelPieceDownloadIsIdempotent covers both idempotent cases: a torrent
// with no piece download, and one whose download already reached a terminal
// state before the cancel arrived.
func TestCancelPieceDownloadIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	work := t.TempDir()
	content := bytes.Repeat([]byte{2}, 2*testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	hashHex := hash.HexString()
	seeder := seederFor(t, ctx, filepath.Join(work, "seeder"), torrentBytes, hash, content)

	if err := seeder.CancelPieceDownload(hashHex); err != nil {
		t.Fatalf("cancel with no piece download ever started = %v, want nil", err)
	}

	if _, err := seeder.StartPieceDownload(ctx, hashHex, []session.PieceRange{{Start: 0, End: 1}}); err != nil {
		t.Fatalf("StartPieceDownload: %v", err)
	}
	status, err := seeder.PieceDownloadStatus(hashHex)
	if err != nil || status.State != session.PieceDownloadCompleted {
		t.Fatalf("status = %+v, err = %v; want an immediately completed download", status, err)
	}

	if err := seeder.CancelPieceDownload(hashHex); err != nil {
		t.Fatalf("cancel after completion = %v, want nil", err)
	}
	after, err := seeder.PieceDownloadStatus(hashHex)
	if err != nil {
		t.Fatalf("PieceDownloadStatus: %v", err)
	}
	if after.State != session.PieceDownloadCompleted {
		t.Fatalf("state after cancelling a completed download = %q, want the terminal state left untouched", after.State)
	}
}

// TestCancelPieceDownloadUnknownTorrent covers the one case cancel is not
// idempotent about: an id with no registered torrent or deletion at all.
func TestCancelPieceDownloadUnknownTorrent(t *testing.T) {
	sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))
	if err := sess.CancelPieceDownload(strings.Repeat("ab", 20)); !errors.Is(err, session.ErrUnknownTorrent) {
		t.Fatalf("Cancel unknown torrent = %v, want ErrUnknownTorrent", err)
	}
}

// TestStartPieceDownloadReplacesInFlightDownload covers the replace rule: a
// second start on the same torrent stops the first and the status reported
// afterwards is the new request's, not a merge of the two.
func TestStartPieceDownloadReplacesInFlightDownload(t *testing.T) {
	ctx := testTimeout(t)
	sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))
	content := bytes.Repeat([]byte{4}, 3*testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("AddTorrentAndPersist: %v", err)
	}
	hashHex := hash.HexString()

	first, err := sess.StartPieceDownload(ctx, hashHex, []session.PieceRange{{Start: 0, End: 3}})
	if err != nil {
		t.Fatalf("first StartPieceDownload: %v", err)
	}
	if first.RequestedPieces != 3 || first.State != session.PieceDownloadRunning {
		t.Fatalf("first status = %+v, want running with 3 requested", first)
	}

	second, err := sess.StartPieceDownload(ctx, hashHex, []session.PieceRange{{Start: 0, End: 1}})
	if err != nil {
		t.Fatalf("second StartPieceDownload: %v", err)
	}
	if second.RequestedPieces != 1 {
		t.Fatalf("second status = %+v, want 1 requested", second)
	}

	status, err := sess.PieceDownloadStatus(hashHex)
	if err != nil {
		t.Fatalf("PieceDownloadStatus: %v", err)
	}
	if status.RequestedPieces != 1 {
		t.Fatalf("status after replacement = %+v, want the second request's 1, not a merge with the first", status)
	}
}

// TestPieceDownloadStatusNeverStarted covers a ready torrent nobody has ever
// asked to piece-download: the status is absent, not an empty running record.
func TestPieceDownloadStatusNeverStarted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	work := t.TempDir()
	content := bytes.Repeat([]byte{9}, 2*testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)

	seeder := seederFor(t, ctx, filepath.Join(work, "seeder"), torrentBytes, hash, content)
	if _, err := seeder.PieceDownloadStatus(hash.HexString()); !errors.Is(err, session.ErrNoPieceDownload) {
		t.Fatalf("PieceDownloadStatus with no piece download ever started = %v, want ErrNoPieceDownload", err)
	}
}
