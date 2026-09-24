package session_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"golang.org/x/time/rate"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

const (
	matrixPieceCount  = 512
	matrixTargetPiece = 8
	matrixRate        = 1 << 20
	matrixBurst       = 128 << 10
)

type rapidSeekCase struct {
	name                 string
	positions            []int
	wantStableGeneration bool
	wantConfirmedSeek    bool
}

func TestSessionRapidSeekPlaybackWindowMatrix(t *testing.T) {
	content := make([]byte, matrixPieceCount*testPieceLength)
	for i := range content {
		content[i] = byte(i*31 + i/251 + 1)
	}

	tracker := newLoopbackTracker(t)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, [][]string{{tracker.url}})
	hashHex := hash.HexString()

	seederDir := filepath.Join(t.TempDir(), "seeder-data")
	seeder := newLoopbackSession(t, testConfig(), seederDir)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := seeder.AddTorrent(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("seeder AddTorrent: %v", err)
	}
	seederHandle, ok := seeder.Torrent(hash)
	if !ok {
		t.Fatal("seeder did not register torrent")
	}
	seedPieces(t, seeder, hash, content)
	waitCached(t, ctx, seederHandle)

	cases := []rapidSeekCase{
		{name: "window stays bounded", positions: []int{8, 16, 32, 64}, wantStableGeneration: true},
		{name: "sparse probes do not chase", positions: []int{8, 200, 350, 480}, wantStableGeneration: true},
		{name: "confirmed seek", positions: []int{8, 200, 201}, wantConfirmedSeek: true},
	}
	for _, seekCase := range cases {
		seekCase := seekCase
		t.Run(seekCase.name, func(t *testing.T) {
			runRapidSeekCase(t, tracker, hashHex, hash, torrentBytes, content, seekCase)
		})
	}
}

func runRapidSeekCase(t *testing.T, tracker *loopbackTracker, hashHex string, hash metainfo.Hash, torrentBytes, content []byte, seekCase rapidSeekCase) {
	t.Helper()
	pieceLength := int64(testPieceLength)
	dataDir := filepath.Join(t.TempDir(), "leecher-data")
	leecher := newLoopbackSessionWithCustomize(t, testConfig(), dataDir, func(cc *session.TorrentClientConfig) {
		cc.DownloadRateLimiter = newMatrixLimiter()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := leecher.AddTorrent(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("leecher AddTorrent: %v", err)
	}
	leecherHandle, ok := leecher.Torrent(hash)
	if !ok {
		t.Fatal("leecher did not register torrent")
	}
	if got := leecherHandle.CachedBytes(); got != 0 {
		t.Fatalf("leecher starts with %d cached bytes; want cold storage", got)
	}
	if err := waitFor(ctx, func() bool { return tracker.peerCount(hashHex) >= 2 }); err != nil {
		t.Fatalf("leecher did not connect to seeder: %v", err)
	}

	reader, err := leecher.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = reader.(io.Closer).Close() }()

	var previous session.PrefetchSnapshot
	for index, piece := range seekCase.positions {
		offset := int64(piece) * pieceLength
		buf := make([]byte, 64)
		started := time.Now()
		n, err := reader.ReadAt(buf, offset)
		if err != nil || n != len(buf) {
			t.Fatalf("seek %d at piece %d = %d bytes, %v; want %d, nil", index, piece, n, err, len(buf))
		}
		if !bytes.Equal(buf, content[int(offset):int(offset)+len(buf)]) {
			t.Fatalf("seek %d at piece %d returned different data", index, piece)
		}
		snapshot := session.PrefetchSnapshotForTest(leecherHandle)
		if snapshot.MaxActivePieces > session.PrefetchDefaultPiecesForTest() {
			t.Fatalf("max active background pieces = %d, want at most %d", snapshot.MaxActivePieces, session.PrefetchDefaultPiecesForTest())
		}
		t.Logf("seek=%d piece=%d elapsed=%s generation=%d anchor=%d candidate=%v/%d window=[%d,%d) foreground=%v active=%v cancels=%d stale_spans=%d", index, piece, time.Since(started), snapshot.Generation, snapshot.PlaybackAnchor, snapshot.CandidatePresent, snapshot.CandidateReads, snapshot.WindowStart, snapshot.WindowEnd, snapshot.ForegroundIndexes, snapshot.ActivePieceIndexes, snapshot.ForegroundCancels, snapshot.StaleSpanRejects)
		if index > 0 && snapshot.Generation < previous.Generation {
			t.Fatalf("generation moved backwards: %d -> %d", previous.Generation, snapshot.Generation)
		}
		if index > 0 && seekCase.wantStableGeneration {
			if snapshot.Generation != previous.Generation {
				t.Fatalf("sparse read changed generation from %d to %d", previous.Generation, snapshot.Generation)
			}
			if snapshot.PlaybackAnchor != previous.PlaybackAnchor || snapshot.WindowStart != previous.WindowStart {
				t.Fatalf("sparse read moved anchor/window from %d/%d to %d/%d", previous.PlaybackAnchor, previous.WindowStart, snapshot.PlaybackAnchor, snapshot.WindowStart)
			}
		}
		if index == len(seekCase.positions)-1 && seekCase.wantConfirmedSeek {
			if snapshot.Generation != previous.Generation+1 {
				t.Fatalf("confirmed seek generation = %d, want %d", snapshot.Generation, previous.Generation+1)
			}
			if snapshot.CandidatePresent {
				t.Fatal("confirmed seek left a candidate installed")
			}
		}
		previous = snapshot
	}
	if previous.ForegroundTickets != 0 {
		t.Fatalf("foreground tickets after final read = %d, want 0", previous.ForegroundTickets)
	}
}

// TestSessionPrefetchBudgetMatrix measures the same cold-start/seek path for
// each candidate background-piece budget (4/6/8), using the Session-global
// token budget as the only knob. It records time-to-first-byte, the forward
// buffer the coordinator built, and how many sequential playback reads stalled,
// so a single value can be chosen from the same conditions the baseline matrix
// used. No wall-clock assertion is made: the numbers are logged for comparison.
func TestSessionPrefetchBudgetMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("cold-seek matrix is a timing measurement")
	}
	content := make([]byte, matrixPieceCount*testPieceLength)
	for i := range content {
		content[i] = byte(i*31 + i/251 + 1)
	}
	tracker := newLoopbackTracker(t)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, [][]string{{tracker.url}})
	hashHex := hash.HexString()

	seederDir := filepath.Join(t.TempDir(), "seeder-data")
	seeder := newLoopbackSession(t, testConfig(), seederDir)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := seeder.AddTorrent(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("seeder AddTorrent: %v", err)
	}
	seederHandle, ok := seeder.Torrent(hash)
	if !ok {
		t.Fatal("seeder did not register torrent")
	}
	seedPieces(t, seeder, hash, content)
	waitCached(t, ctx, seederHandle)

	for _, limit := range []int{4, 6, 8} {
		limit := limit
		t.Run(fmt.Sprintf("active=%d", limit), func(t *testing.T) {
			runPrefetchBudgetCandidate(t, tracker, hashHex, hash, torrentBytes, content, limit)
		})
	}
}

func runPrefetchBudgetCandidate(t *testing.T, tracker *loopbackTracker, hashHex string, hash metainfo.Hash, torrentBytes, content []byte, limit int) {
	t.Helper()
	pieceLength := int64(testPieceLength)
	dataDir := filepath.Join(t.TempDir(), "leecher-data")
	leecher := newLoopbackSessionWithCustomize(t, testConfig(), dataDir, func(cc *session.TorrentClientConfig) {
		cc.DownloadRateLimiter = newMatrixLimiter()
	})
	restore := session.SetPrefetchBudgetLimitForTest(leecher, limit)
	defer restore()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := leecher.AddTorrent(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("leecher AddTorrent: %v", err)
	}
	leecherHandle, ok := leecher.Torrent(hash)
	if !ok {
		t.Fatal("leecher did not register torrent")
	}
	if err := waitFor(ctx, func() bool { return tracker.peerCount(hashHex) >= 2 }); err != nil {
		t.Fatalf("leecher did not connect to seeder: %v", err)
	}
	reader, err := leecher.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = reader.(io.Closer).Close() }()

	targetOffset := int64(matrixTargetPiece) * pieceLength
	buf := make([]byte, 64)
	started := time.Now()
	if n, err := reader.ReadAt(buf, targetOffset); err != nil || n != len(buf) {
		t.Fatalf("cold read = (%d, %v), want (%d, nil)", n, err, len(buf))
	}
	firstByte := time.Since(started)

	snapshot := session.PrefetchSnapshotForTest(leecherHandle)
	if snapshot.MaxActivePieces > limit {
		t.Fatalf("max active background pieces = %d, want at most %d", snapshot.MaxActivePieces, limit)
	}

	const playbackPieces = 6
	const stallThreshold = 100 * time.Millisecond
	stalls := 0
	var playback time.Duration
	for piece := matrixTargetPiece + 1; piece <= matrixTargetPiece+playbackPieces; piece++ {
		off := int64(piece) * pieceLength
		readBuf := make([]byte, 64)
		readStart := time.Now()
		n, err := reader.ReadAt(readBuf, off)
		duration := time.Since(readStart)
		playback += duration
		if duration >= stallThreshold {
			stalls++
		}
		if err != nil || n != len(readBuf) {
			t.Fatalf("playback piece %d = (%d, %v), want (%d, nil)", piece, n, err, len(readBuf))
		}
		if !bytes.Equal(readBuf, content[int(off):int(off)+len(readBuf)]) {
			t.Fatalf("playback piece %d data differs from source", piece)
		}
	}
	after := session.PrefetchSnapshotForTest(leecherHandle)
	t.Logf("active=%d ttfb=%s buffered=%d max_active=%d priority_adds=%d priority_cancels=%d dedupe=%d budget_blocks=%d playback=%s stalls_ge_%s=%d/%d",
		limit, firstByte, snapshot.BufferedBytes, snapshot.MaxActivePieces,
		after.PriorityAdds, after.PriorityCancels, after.DedupeCount, after.BudgetBlocks,
		playback, stallThreshold, stalls, playbackPieces)
}

func newMatrixLimiter() *rate.Limiter {
	return rate.NewLimiter(rate.Limit(matrixRate), matrixBurst)
}

func waitForPiecePriority(ctx context.Context, st *session.Torrent, piece int, want torrent.PiecePriority) (time.Time, error) {
	for {
		if got := piecePriorityAt(session.PieceStateRunsForTest(st), piece); got >= want {
			return time.Now(), nil
		}
		select {
		case <-ctx.Done():
			return time.Time{}, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func piecePriorityAt(runs torrent.PieceStateRuns, piece int) torrent.PiecePriority {
	start := 0
	for _, run := range runs {
		if piece < start+run.Length {
			return run.Priority
		}
		start += run.Length
	}
	return torrent.PiecePriorityNone
}
