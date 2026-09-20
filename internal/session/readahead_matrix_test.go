package session_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"golang.org/x/time/rate"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

const (
	matrixPieceCount  = 192
	matrixTargetPiece = 8
	matrixRate        = 1 << 20
	matrixBurst       = 128 << 10
)

type readaheadCandidate struct {
	name  string
	bytes int64
}

type seekPosition struct {
	name   string
	offset int64
}

func TestSessionColdSeekReadaheadMatrix(t *testing.T) {
	content := make([]byte, matrixPieceCount*testPieceLength)
	for i := range content {
		content[i] = byte(i*31 + i/251 + 1)
	}

	tracker := newLoopbackTracker(t)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, [][]string{{tracker.url}})
	hashHex := hash.HexString()

	seederDir := filepath.Join(t.TempDir(), "seeder-data")
	seeder := newLoopbackSession(t, testConfig(), seederDir)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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

	pieceLength := int64(testPieceLength)
	candidates := []readaheadCandidate{
		{name: "piece-length baseline", bytes: pieceLength},
		{name: "8 MiB", bytes: 8 << 20},
		{name: "16 MiB", bytes: 16 << 20},
		{name: "32 MiB", bytes: 32 << 20},
	}
	positions := []seekPosition{
		{name: "piece boundary", offset: 0},
		{name: "piece middle", offset: pieceLength / 2},
	}
	caseNumber := 0
	for _, position := range positions {
		position := position
		for _, candidate := range candidates {
			candidate := candidate
			caseNumber++
			t.Run(position.name+"/"+candidate.name, func(t *testing.T) {
				runColdSeekCandidate(t, tracker, hashHex, hash, torrentBytes, content, pieceLength, position.offset, candidate, caseNumber)
			})
		}
	}
}

func runColdSeekCandidate(t *testing.T, tracker *loopbackTracker, hashHex string, hash metainfo.Hash, torrentBytes, content []byte, pieceLength, positionOffset int64, candidate readaheadCandidate, caseNumber int) {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "leecher-data")
	leecher := newLoopbackSessionWithCustomize(t, testConfig(), dataDir, func(cc *session.TorrentClientConfig) {
		cc.DownloadRateLimiter = newMatrixLimiter()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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
	if !session.SetReadaheadForTest(reader, candidate.bytes) {
		t.Fatal("OpenFile did not return a session raFile")
	}

	targetOffset := int64(matrixTargetPiece)*pieceLength + positionOffset
	firstRead := make([]byte, 64)
	seekStarted := time.Now()
	readDone := make(chan struct {
		n    int
		err  error
		data []byte
	}, 1)
	go func() {
		n, err := reader.ReadAt(firstRead, targetOffset)
		readDone <- struct {
			n    int
			err  error
			data []byte
		}{n: n, err: err, data: append([]byte(nil), firstRead...)}
	}()

	priorityAt, err := waitForPiecePriority(ctx, leecherHandle, matrixTargetPiece, torrent.PiecePriorityNow)
	if err != nil {
		t.Fatalf("target priority was not established: %v", err)
	}
	var result struct {
		n    int
		err  error
		data []byte
	}
	select {
	case result = <-readDone:
	case <-ctx.Done():
		t.Fatalf("first cold read: %v", ctx.Err())
	}
	firstDataAt := time.Now()
	if result.err != nil || result.n != len(firstRead) {
		t.Fatalf("first cold read = %d bytes, %v; want %d, nil", result.n, result.err, len(firstRead))
	}
	if want := content[int(targetOffset) : int(targetOffset)+len(firstRead)]; !bytes.Equal(result.data, want) {
		t.Fatal("first cold read data differs from source")
	}

	readPosition := targetOffset + int64(result.n)
	windowEnd := int((readPosition + candidate.bytes + pieceLength - 1) / pieceLength)
	forwardCompleted := completedPiecesInRange(session.PieceStateRunsForTest(leecherHandle), matrixTargetPiece+1, windowEnd)
	completedAtFirst := leecherHandle.CachedBytes()
	t.Logf("case=%d priority=%s first_data=%s priority_to_first=%s completed_at_first=%d forward_completed=%d forward_bytes=%d", caseNumber, priorityAt.Sub(seekStarted), firstDataAt.Sub(seekStarted), firstDataAt.Sub(priorityAt), completedAtFirst, forwardCompleted, int64(forwardCompleted)*pieceLength)

	const playbackPieces = 6
	const stallThreshold = 100 * time.Millisecond
	stalls := 0
	var playback time.Duration
	for piece := matrixTargetPiece + 1; piece <= matrixTargetPiece+playbackPieces; piece++ {
		off := int64(piece) * pieceLength
		buf := make([]byte, 64)
		started := time.Now()
		n, err := reader.ReadAt(buf, off)
		duration := time.Since(started)
		playback += duration
		if duration >= stallThreshold {
			stalls++
		}
		if err != nil || n != len(buf) {
			t.Fatalf("sequential piece %d read = %d bytes, %v; want %d, nil", piece, n, err, len(buf))
		}
		if !bytes.Equal(buf, content[int(off):int(off)+len(buf)]) {
			t.Fatalf("sequential piece %d data differs from source", piece)
		}
	}
	t.Logf("playback_duration=%s stalls_ge_%s=%d/%d", playback, stallThreshold, stalls, playbackPieces)
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

func completedPiecesInRange(runs torrent.PieceStateRuns, begin, end int) int {
	if end <= begin {
		return 0
	}
	start := 0
	completed := 0
	for _, run := range runs {
		runEnd := start + run.Length
		overlapStart := max(start, begin)
		overlapEnd := min(runEnd, end)
		if overlapStart < overlapEnd && run.Completion.Ok {
			completed += overlapEnd - overlapStart
		}
		start = runEnd
		if start >= end {
			break
		}
	}
	return completed
}
