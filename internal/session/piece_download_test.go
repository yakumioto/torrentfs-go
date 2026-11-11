package session_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/netip"
	"path/filepath"
	"runtime"
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

	status, err := leecher.StartPieceDownload(ctx, hashHex, []session.PieceRange{{Start: 0, End: 1}}, 0)
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

	status, err := seeder.StartPieceDownload(ctx, hashHex, []session.PieceRange{{Start: 0, End: 2}}, 0)
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
	}, 0)
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
			_, err := seeder.StartPieceDownload(ctx, hashHex, tt.ranges, 0)
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
	if _, err := sess.StartPieceDownload(ctx, bogus, []session.PieceRange{{Start: 0, End: 1}}, 0); !errors.Is(err, session.ErrUnknownTorrent) {
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
	if _, err := sess.StartPieceDownload(ctx, hash.HexString(), []session.PieceRange{{Start: 0, End: 1}}, 0); !errors.Is(err, session.ErrDeleting) {
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
	if _, err := sess.StartPieceDownload(ctx, added.ID, []session.PieceRange{{Start: 0, End: 1}}, 0); !errors.Is(err, session.ErrTorrentNotReady) {
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

	status, err := sess.StartPieceDownload(ctx, hashHex, []session.PieceRange{{Start: 0, End: 2}}, 0)
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

	if _, err := seeder.StartPieceDownload(ctx, hashHex, []session.PieceRange{{Start: 0, End: 1}}, 0); err != nil {
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

	first, err := sess.StartPieceDownload(ctx, hashHex, []session.PieceRange{{Start: 0, End: 3}}, 0)
	if err != nil {
		t.Fatalf("first StartPieceDownload: %v", err)
	}
	if first.RequestedPieces != 3 || first.State != session.PieceDownloadRunning {
		t.Fatalf("first status = %+v, want running with 3 requested", first)
	}

	second, err := sess.StartPieceDownload(ctx, hashHex, []session.PieceRange{{Start: 0, End: 1}}, 0)
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

// TestPieceDownloadExpiresWhenTimeoutElapsesWithNoPeer covers the time
// limit: a download with no reachable peer is cut off by its own deadline
// instead of hanging forever, moves to the expired terminal state, and the
// torrent's reading allowance falls back the same way it does once a
// download completes.
func TestPieceDownloadExpiresWhenTimeoutElapsesWithNoPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := testConfig()
	sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))
	_, advance := fakeConnectionClock(t, sess)
	content := bytes.Repeat([]byte{7}, 2*testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("AddTorrentAndPersist: %v", err)
	}
	hashHex := hash.HexString()
	state, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent did not register")
	}

	status, err := sess.StartPieceDownload(ctx, hashHex, []session.PieceRange{{Start: 0, End: 2}}, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("StartPieceDownload: %v", err)
	}
	if status.State != session.PieceDownloadRunning {
		t.Fatalf("state = %q, want running", status.State)
	}

	if err := waitFor(ctx, func() bool {
		s, err := sess.PieceDownloadStatus(hashHex)
		return err == nil && s.State == session.PieceDownloadExpired
	}); err != nil {
		t.Fatalf("piece download never expired: %v", err)
	}
	final, err := sess.PieceDownloadStatus(hashHex)
	if err != nil {
		t.Fatalf("PieceDownloadStatus: %v", err)
	}
	if final.DonePieces != 0 {
		t.Fatalf("done_pieces = %d, want 0 (no peer ever served a piece)", final.DonePieces)
	}
	if final.FinishedAt.IsZero() {
		t.Fatal("finished_at is zero after expiry")
	}

	// The reading allowance an expired download held while running must
	// fall back once it is no longer in flight: still reading right after
	// (the grace window is still open), then idle once it elapses, since
	// nothing was ever fetched into the cache to justify a probe connection.
	session.AllocateConnectionsForTest(sess)
	if got := session.ConnectionStateForTest(state); got != "reading" {
		t.Fatalf("torrent right after its piece download expired is in state %q, want reading", got)
	}
	advance(time.Duration(cfg.Connections.ReadGrace) + time.Second)
	session.AllocateConnectionsForTest(sess)
	if got := session.ConnectionStateForTest(state); got != "idle" {
		t.Fatalf("torrent past the read grace after its piece download expired is in state %q, want idle (nothing was ever cached)", got)
	}
}

// TestPieceDownloadExpiresPreservesPartiallyFetchedPieces covers the other
// half of the time limit rule: a piece genuinely fetched over the wire
// before the peer goes away stays done, and stays cached, once the rest of
// the request expires.
func TestPieceDownloadExpiresPreservesPartiallyFetchedPieces(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	work := t.TempDir()
	content := bytes.Repeat([]byte{11}, 3*testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	hashHex := hash.HexString()

	seeder := seederFor(t, ctx, filepath.Join(work, "seeder"), torrentBytes, hash, content)
	seederAddr := torrent.PeerInfo{
		Addr: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(seeder.EffectiveListenPort())),
	}

	leecher := newLoopbackSessionWithCustomize(t, testConfig(), filepath.Join(work, "leecher"),
		func(cc *session.TorrentClientConfig) { cc.DropMutuallyCompletePeers = false })
	if err := leecher.AddTorrent(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("leecher AddTorrent: %v", err)
	}
	state, ok := leecher.Torrent(hash)
	if !ok {
		t.Fatal("leecher did not register the torrent")
	}
	session.UnderlyingTorrentForTest(state).AddPeers([]torrent.PeerInfo{seederAddr})

	// Fetch piece 0 for real while the seeder is still reachable.
	if _, err := leecher.StartPieceDownload(ctx, hashHex, []session.PieceRange{{Start: 0, End: 1}}, 0); err != nil {
		t.Fatalf("first StartPieceDownload: %v", err)
	}
	if err := waitFor(ctx, func() bool {
		s, err := leecher.PieceDownloadStatus(hashHex)
		return err == nil && s.State == session.PieceDownloadCompleted
	}); err != nil {
		t.Fatalf("piece 0 never completed: %v", err)
	}

	// Take the only peer away: with no tracker the leecher cannot find
	// another, so the next request has nothing left to serve the rest.
	if err := seeder.Close(context.Background()); err != nil {
		t.Fatalf("seeder Close: %v", err)
	}

	status, err := leecher.StartPieceDownload(ctx, hashHex, []session.PieceRange{{Start: 0, End: 3}}, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("second StartPieceDownload: %v", err)
	}
	if status.RequestedPieces != 3 || status.DonePieces != 1 {
		t.Fatalf("status right after starting = %+v, want 1 already done of 3 requested", status)
	}

	if err := waitFor(ctx, func() bool {
		s, err := leecher.PieceDownloadStatus(hashHex)
		return err == nil && s.State == session.PieceDownloadExpired
	}); err != nil {
		t.Fatalf("piece download never expired: %v", err)
	}
	final, err := leecher.PieceDownloadStatus(hashHex)
	if err != nil {
		t.Fatalf("PieceDownloadStatus: %v", err)
	}
	if final.DonePieces != 1 {
		t.Fatalf("done_pieces after expiry = %d, want 1 (the piece fetched before the peer went away)", final.DonePieces)
	}

	statusView, err := leecher.TorrentStatusFor(hashHex)
	if err != nil {
		t.Fatalf("TorrentStatusFor: %v", err)
	}
	if !statusView.Pieces[0].Cached {
		t.Fatal("piece 0 is no longer cached after the download expired")
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

// TestPieceDownloadCancelledAndDiscardedWhenTorrentDeleted covers the
// delete-time teardown: a piece download with no reachable peer would hang
// forever, but deleting its torrent does not wait for it. Deletion
// completes, the deleted hash no longer has a piece download to query
// (ErrUnknownTorrent, not a leftover record), and re-adding the same
// torrent starts from a clean slate instead of resurrecting the discarded
// one.
func TestPieceDownloadCancelledAndDiscardedWhenTorrentDeleted(t *testing.T) {
	ctx := testTimeout(t)
	torrentsDir := testTorrentDir(t, filepath.Join(t.TempDir(), "data"))
	sess := newManageSession(t, torrentsDir)
	content := bytes.Repeat([]byte{12}, 2*testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	added, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes})
	if err != nil {
		t.Fatalf("AddTorrentAndPersist: %v", err)
	}
	hashHex := hash.HexString()

	status, err := sess.StartPieceDownload(ctx, hashHex, []session.PieceRange{{Start: 0, End: 2}}, 0)
	if err != nil {
		t.Fatalf("StartPieceDownload: %v", err)
	}
	if status.State != session.PieceDownloadRunning {
		t.Fatalf("state = %q, want running (no peer can have served it yet)", status.State)
	}

	op, err := sess.DeleteTorrent(ctx, added.ID)
	if err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}
	final := waitOperation(t, sess, op.ID)
	if final.State != session.StateDeleted {
		t.Fatalf("delete state = %s (%s), want deleted; a piece download with no peer must not block it", final.State, final.Error)
	}

	if _, err := sess.PieceDownloadStatus(hashHex); !errors.Is(err, session.ErrUnknownTorrent) {
		t.Fatalf("PieceDownloadStatus after delete = %v, want ErrUnknownTorrent", err)
	}

	readded, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes})
	if err != nil {
		t.Fatalf("re-add after delete: %v", err)
	}
	if _, err := sess.PieceDownloadStatus(readded.ID); !errors.Is(err, session.ErrNoPieceDownload) {
		t.Fatalf("PieceDownloadStatus on the re-added torrent = %v, want ErrNoPieceDownload (the discarded download must not resurface)", err)
	}
}

// TestPieceDownloadInFlightReleasesGoroutinesAndFileDescriptorsOnDelete
// mirrors TestDeleteReleasesGoroutinesAndFileDescriptors with a piece
// download in flight at delete time: its background fetch loop must not
// outlive the torrent it was fetching for.
func TestPieceDownloadInFlightReleasesGoroutinesAndFileDescriptorsOnDelete(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("file descriptor accounting requires /proc")
	}
	ctx := testTimeout(t)
	work := t.TempDir()
	content := bytes.Repeat([]byte{13}, 2*testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	hashHex := hash.HexString()

	baselineGoroutines := runtime.NumGoroutine()
	baselineFDs := openFDCount(t)

	settle := func() (int, int) {
		deadline := time.Now().Add(5 * time.Second)
		var goroutines, fds int
		for {
			runtime.GC()
			goroutines = runtime.NumGoroutine()
			fds = openFDCount(t)
			if time.Now().After(deadline) {
				return goroutines, fds
			}
			if goroutines <= baselineGoroutines+2 && fds <= baselineFDs+2 {
				return goroutines, fds
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	sess, err := session.New(testConfig(), testTorrentDir(t, filepath.Join(work, "data")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	view, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if status, err := sess.StartPieceDownload(ctx, hashHex, []session.PieceRange{{Start: 0, End: 2}}, 0); err != nil {
		t.Fatalf("StartPieceDownload: %v", err)
	} else if status.State != session.PieceDownloadRunning {
		t.Fatalf("state = %q, want running (no peer can have served it yet)", status.State)
	}
	op, err := sess.DeleteTorrent(ctx, view.ID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if final := waitOperation(t, sess, op.ID); final.State != session.StateDeleted {
		t.Fatalf("delete state = %s (%s), want deleted", final.State, final.Error)
	}
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	goroutines, fds := settle()
	if goroutines > baselineGoroutines+2 {
		t.Fatalf("goroutines after add/piece-download/delete/close = %d, baseline %d", goroutines, baselineGoroutines)
	}
	if fds > baselineFDs+2 {
		t.Fatalf("open file descriptors after add/piece-download/delete/close = %d, baseline %d", fds, baselineFDs)
	}
}

// TestPieceDownloadInFlightDoesNotBlockSessionClose covers the session-level
// teardown: a piece download with no reachable peer would hang forever, but
// closing the session does not wait for it, returns promptly, and leaves
// nothing running. A fresh session opened on the same directory afterwards
// (standing in for a process restart) resumes the torrent but has no memory
// of the piece download: it was never persisted.
func TestPieceDownloadInFlightDoesNotBlockSessionClose(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	content := bytes.Repeat([]byte{14}, 2*testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	hashHex := hash.HexString()

	checkFDs := runtime.GOOS == "linux"
	baselineGoroutines := runtime.NumGoroutine()
	var baselineFDs int
	if checkFDs {
		baselineFDs = openFDCount(t)
	}

	sess, err := session.New(testConfig(), torrentsDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("AddTorrentAndPersist: %v", err)
	}
	status, err := sess.StartPieceDownload(ctx, hashHex, []session.PieceRange{{Start: 0, End: 2}}, 0)
	if err != nil {
		t.Fatalf("StartPieceDownload: %v", err)
	}
	if status.State != session.PieceDownloadRunning {
		t.Fatalf("state = %q, want running (no peer can have served it yet)", status.State)
	}

	closed := make(chan error, 1)
	start := time.Now()
	go func() { closed <- sess.Close(context.Background()) }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close with a piece download in flight did not return within 5s")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Close took %s with a piece download in flight, want it to not wait for a hung download", elapsed)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		goroutines := runtime.NumGoroutine()
		fds := baselineFDs
		if checkFDs {
			fds = openFDCount(t)
		}
		if goroutines <= baselineGoroutines+2 && fds <= baselineFDs+2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines/fds after closing with a piece download in flight = %d/%d, baseline %d/%d", goroutines, fds, baselineGoroutines, baselineFDs)
		}
		time.Sleep(20 * time.Millisecond)
	}

	restarted, err := session.New(testConfig(), torrentsDir)
	if err != nil {
		t.Fatalf("New on restart: %v", err)
	}
	defer func() {
		if err := restarted.Close(context.Background()); err != nil {
			t.Errorf("restarted Close: %v", err)
		}
	}()
	var statusErr error
	if err := waitFor(ctx, func() bool {
		_, statusErr = restarted.PieceDownloadStatus(hashHex)
		return !errors.Is(statusErr, session.ErrUnknownTorrent)
	}); err != nil {
		t.Fatalf("torrent never resumed after restart: %v", err)
	}
	if !errors.Is(statusErr, session.ErrNoPieceDownload) {
		t.Fatalf("PieceDownloadStatus after restart = %v, want ErrNoPieceDownload (no persisted record)", statusErr)
	}
}

// TestPieceDownloadTwoTorrentsCompleteConcurrently covers a session-wide
// budget under real contention: two downloads whose pieces together exceed
// the budget must both make progress and finish, neither starving the other.
func TestPieceDownloadTwoTorrentsCompleteConcurrently(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	work := t.TempDir()
	type feed struct {
		name    string
		content []byte
		hash    metainfo.Hash
		hashHex string
		peers   []torrent.PeerInfo
	}
	feeds := make([]feed, 0, 2)
	for i, name := range []string{"first.bin", "second.bin"} {
		content := bytes.Repeat([]byte{byte(20 + i)}, 6*testPieceLength)
		torrentBytes, hash := buildSingleFileTorrentBytes(t, name, content, nil)
		seeder := seederFor(t, ctx, filepath.Join(work, name+"-seeder"), torrentBytes, hash, content)
		feeds = append(feeds, feed{
			name:    name,
			content: content,
			hash:    hash,
			hashHex: hash.HexString(),
			peers: []torrent.PeerInfo{
				{Addr: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(seeder.EffectiveListenPort()))},
			},
		})
	}

	leecher := newLoopbackSessionWithCustomize(t, testConfig(), filepath.Join(work, "leecher"),
		func(cc *session.TorrentClientConfig) { cc.DropMutuallyCompletePeers = false })
	for _, f := range feeds {
		torrentBytes, _ := buildSingleFileTorrentBytes(t, f.name, f.content, nil)
		if err := leecher.AddTorrent(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
			t.Fatalf("%s leecher AddTorrent: %v", f.name, err)
		}
		state, ok := leecher.Torrent(f.hash)
		if !ok {
			t.Fatalf("%s was not registered", f.name)
		}
		session.UnderlyingTorrentForTest(state).AddPeers(f.peers)
	}

	for _, f := range feeds {
		if _, err := leecher.StartPieceDownload(ctx, f.hashHex, []session.PieceRange{{Start: 0, End: 6}}, 0); err != nil {
			t.Fatalf("%s StartPieceDownload: %v", f.name, err)
		}
	}
	for _, f := range feeds {
		f := f
		if err := waitFor(ctx, func() bool {
			status, err := leecher.PieceDownloadStatus(f.hashHex)
			if err != nil {
				t.Fatalf("%s PieceDownloadStatus: %v", f.name, err)
			}
			return status.State == session.PieceDownloadCompleted
		}); err != nil {
			t.Fatalf("%s download never completed alongside the other: %v", f.name, err)
		}
		state, ok := leecher.Torrent(f.hash)
		if !ok {
			t.Fatalf("%s disappeared", f.name)
		}
		if got, want := state.CachedBytes(), state.Length(); got != want {
			t.Fatalf("%s cached %d bytes, want %d", f.name, got, want)
		}
	}
}

// TestPieceDownloadConcurrentWithReadingTheSameFile covers the shared-flight
// path: reading a file while a piece download for the same torrent is in
// flight returns the original content, because both go through the same piece
// fetcher and a piece in flight is fetched once for every waiter.
func TestPieceDownloadConcurrentWithReadingTheSameFile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	work := t.TempDir()
	content := bytes.Repeat([]byte{42}, 4*testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "shared.bin", content, nil)

	seeder := seederFor(t, ctx, filepath.Join(work, "seeder"), torrentBytes, hash, content)
	leecher := newLoopbackSessionWithCustomize(t, testConfig(), filepath.Join(work, "leecher"),
		func(cc *session.TorrentClientConfig) { cc.DropMutuallyCompletePeers = false })
	if err := leecher.AddTorrent(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("leecher AddTorrent: %v", err)
	}
	state, ok := leecher.Torrent(hash)
	if !ok {
		t.Fatal("leecher did not register the torrent")
	}
	session.UnderlyingTorrentForTest(state).AddPeers([]torrent.PeerInfo{
		{Addr: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(seeder.EffectiveListenPort()))},
	})

	if _, err := leecher.StartPieceDownload(ctx, hash.HexString(), []session.PieceRange{{Start: 0, End: 4}}, 0); err != nil {
		t.Fatalf("StartPieceDownload: %v", err)
	}
	file, err := leecher.OpenFile(hash, "shared.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = file.(io.Closer).Close() }()
	got := make([]byte, len(content))
	for off := 0; off < len(got); off += 32 << 10 {
		end := off + 32<<10
		if end > len(got) {
			end = len(got)
		}
		if _, err := file.ReadAt(got[off:end], int64(off)); err != nil && err != io.EOF {
			t.Fatalf("ReadAt(%d): %v", off, err)
		}
	}
	if !bytes.Equal(got, content) {
		t.Fatal("reading during a piece download returned content that differs from the original")
	}
	if err := waitFor(ctx, func() bool {
		status, err := leecher.PieceDownloadStatus(hash.HexString())
		if err != nil {
			t.Fatalf("PieceDownloadStatus: %v", err)
		}
		return status.State == session.PieceDownloadCompleted
	}); err != nil {
		t.Fatalf("download never completed: %v", err)
	}
}

// TestPieceDownloadDoesNotConsumeThePlaybackReadBudget covers the separation
// between the two budgets: one torrent's piece download holds every slot of
// its own budget while another torrent's file is read, and the read still
// finishes with the original content. Cancelling the download then frees the
// slots, which a second real download on the read torrent proves by
// completing.
func TestPieceDownloadDoesNotConsumeThePlaybackReadBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	work := t.TempDir()
	noPeerContent := bytes.Repeat([]byte{7}, 9*testPieceLength)
	noPeerBytes, noPeerHash := buildSingleFileTorrentBytes(t, "no-peer.bin", noPeerContent, nil)
	readContent := bytes.Repeat([]byte{8}, 3*testPieceLength)
	readBytes, readHash := buildSingleFileTorrentBytes(t, "read.bin", readContent, nil)

	seeder := seederFor(t, ctx, filepath.Join(work, "seeder"), readBytes, readHash, readContent)
	leecher := newLoopbackSessionWithCustomize(t, testConfig(), filepath.Join(work, "leecher"),
		func(cc *session.TorrentClientConfig) { cc.DropMutuallyCompletePeers = false })
	for _, metainfoBytes := range [][]byte{noPeerBytes, readBytes} {
		if err := leecher.AddTorrent(ctx, session.Source{Metainfo: metainfoBytes}); err != nil {
			t.Fatalf("leecher AddTorrent: %v", err)
		}
	}
	readState, ok := leecher.Torrent(readHash)
	if !ok {
		t.Fatal("leecher did not register the read torrent")
	}
	session.UnderlyingTorrentForTest(readState).AddPeers([]torrent.PeerInfo{
		{Addr: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(seeder.EffectiveListenPort()))},
	})

	// Nine pieces against a budget of eight: every slot is held by fetches
	// that will never finish, since this torrent has no peer.
	if _, err := leecher.StartPieceDownload(ctx, noPeerHash.HexString(), []session.PieceRange{{Start: 0, End: 9}}, 0); err != nil {
		t.Fatalf("StartPieceDownload: %v", err)
	}

	file, err := leecher.OpenFile(readHash, "read.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	got := make([]byte, len(readContent))
	readDone := make(chan error, 1)
	go func() {
		defer func() { _ = file.(io.Closer).Close() }()
		for off := 0; off < len(got); off += 32 << 10 {
			end := off + 32<<10
			if end > len(got) {
				end = len(got)
			}
			if _, err := file.ReadAt(got[off:end], int64(off)); err != nil && err != io.EOF {
				readDone <- err
				return
			}
		}
		readDone <- nil
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("read while the piece download budget was saturated: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("read stalled while another torrent's piece download saturated its own budget")
	}
	if !bytes.Equal(got, readContent) {
		t.Fatal("read returned content that differs from the original")
	}

	// Cancelling the download must return every slot it held, so a real
	// download on the read torrent can then take the whole budget.
	if err := leecher.CancelPieceDownload(noPeerHash.HexString()); err != nil {
		t.Fatalf("CancelPieceDownload: %v", err)
	}
	if _, err := leecher.StartPieceDownload(ctx, readHash.HexString(), []session.PieceRange{{Start: 0, End: 3}}, 0); err != nil {
		t.Fatalf("second StartPieceDownload: %v", err)
	}
	if err := waitFor(ctx, func() bool {
		status, err := leecher.PieceDownloadStatus(readHash.HexString())
		if err != nil {
			t.Fatalf("PieceDownloadStatus: %v", err)
		}
		return status.State == session.PieceDownloadCompleted
	}); err != nil {
		t.Fatalf("budget slots were not returned after cancel: %v", err)
	}
}
