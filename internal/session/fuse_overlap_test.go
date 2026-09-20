package session_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

const (
	// overlapDownloadRate throttles the leecher so a whole piece cannot arrive
	// inside the assertion window, while still finishing in a few seconds. The
	// limiter throttles by sleeping, so it cannot be re-raised mid-transfer.
	overlapDownloadRate = 64 << 10
	// overlapLimiterBurst must be at least one received chunk; anacrolix caps
	// each limited read to the burst.
	overlapLimiterBurst = 16 << 10
	// overlapReadTimeout is generous because the throttled transfer, not the
	// test logic, decides when the reads finish.
	overlapReadTimeout = 60 * time.Second
)

// TestFuseIncompleteOverlapReadsShareOneLoader drives two overlapping reads of
// two still-missing pieces through a real FUSE mount while a throttled swarm
// keeps them outstanding. It proves the whole chain at once: the FUSE bridge
// hands both requests to one shared raFile/loader, the second request does not
// cancel the first, both eventually return their own bytes, and the playback
// phase produces no anacrolix cancellation logs.
func TestFuseIncompleteOverlapReadsShareOneLoader(t *testing.T) {
	requireFuse(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	work := t.TempDir()
	tracker := newLoopbackTracker(t)

	content := make([]byte, 3*testPieceLength+4096)
	for i := range content {
		content[i] = byte(i*29 + i/211)
	}
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, [][]string{{tracker.url}})
	hashHex := hash.HexString()

	// Seeder: its in-memory cache is filled directly.
	seederDir := filepath.Join(work, "seeder-data")
	seeder := newLoopbackSession(t, testConfig(), seederDir)
	if err := seeder.AddTorrent(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("seeder AddTorrent: %v", err)
	}
	seederHandle, ok := seeder.Torrent(hash)
	if !ok {
		t.Fatal("seeder did not register the torrent")
	}
	seedPieces(t, seeder, hash, content)
	waitCached(t, ctx, seederHandle)

	// Leecher: empty storage, a download limiter that keeps the first read
	// outstanding through the millisecond-scale assertion window, and a
	// phase-labelled log capture. The transfer stays bounded so both reads
	// finish on their own: the limiter cannot simply be re-raised mid-transfer,
	// because it throttles by sleeping.
	handler, logState := newPhaseLogHandler()
	limiter := rate.NewLimiter(overlapDownloadRate, overlapLimiterBurst)
	leecherDir := filepath.Join(work, "leecher-data")
	leecher := newLoopbackSessionWithCustomize(t, testConfig(), leecherDir, func(cc *session.TorrentClientConfig) {
		cc.DownloadRateLimiter = limiter
		cc.Slogger = slog.New(handler)
	})
	if err := leecher.AddTorrent(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("leecher AddTorrent: %v", err)
	}
	leecherHandle, ok := leecher.Torrent(hash)
	if !ok {
		t.Fatal("leecher did not register the torrent")
	}
	if got := leecherHandle.CachedBytes(); got != 0 {
		t.Fatalf("leecher starts with %d cached bytes; this test must transfer everything", got)
	}

	// Both peers must be connected before the reads start, so lifting the
	// limiter later is enough to finish the transfer.
	if err := waitFor(ctx, func() bool { return tracker.announceCount(hashHex) >= 2 }); err != nil {
		t.Fatalf("tracker never saw both peers announce: %v", err)
	}
	if err := waitFor(ctx, func() bool { return tracker.peerCount(hashHex) >= 2 }); err != nil {
		t.Fatalf("peers never connected: %v", err)
	}

	recorder := &recordingBackend{inner: leecher}
	mnt := filepath.Join(work, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatalf("make mountpoint: %v", err)
	}
	server, err := filesystem.Mount(mnt, recorder, nil)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(func() { unmountServer(t, server, mnt) })

	path := filepath.Join(mnt, "payload.bin")
	firstFile, err := os.Open(path)
	if err != nil {
		t.Fatalf("open first descriptor: %v", err)
	}
	defer func() { _ = firstFile.Close() }()
	secondFile, err := os.Open(path)
	if err != nil {
		t.Fatalf("open second descriptor: %v", err)
	}
	defer func() { _ = secondFile.Close() }()

	// Both FUSE opens must have landed on one session reader sharing one
	// loader: that shared state is what the cancellation bug was about.
	opened := waitOpenedReaders(t, recorder, 2)
	sameFile, sameLoader := session.SameRAFile(opened[0], opened[1])
	if !sameFile {
		t.Fatal("the two FUSE opens did not share one session reader")
	}
	if !sameLoader {
		t.Fatal("the two FUSE opens did not share one piece loader")
	}

	status, err := leecher.TorrentStatusFor(hashHex)
	if err != nil {
		t.Fatalf("TorrentStatusFor: %v", err)
	}
	firstPiece := 1
	secondPiece := 2
	if firstPiece >= len(status.Pieces) || secondPiece >= len(status.Pieces) {
		t.Fatalf("torrent has %d pieces, need at least %d", len(status.Pieces), secondPiece+1)
	}
	for _, piece := range []int{firstPiece, secondPiece} {
		if status.Pieces[piece].Cached {
			t.Fatalf("piece %d is already cached; the read would not block", piece)
		}
	}

	events := make(chan session.ReadProbeEvent, 32)
	restoreProbe := session.SetReadProbe(events)
	defer restoreProbe()

	// Playback begins here: from now until the phase switches to closing, no
	// reader cancellation may be logged.
	logState.phase.Store(logPhasePlayback)

	firstOffset := int64(firstPiece) * testPieceLength
	secondOffset := int64(secondPiece) * testPieceLength

	firstDone := readAtAsync(firstFile, firstOffset+64, 64)
	firstStarted := waitProbeEvent(t, events, "reader-started", firstOffset)

	secondDone := readAtAsync(secondFile, secondOffset+64, 64)
	waitProbeEvent(t, events, "admission-attempt", secondOffset)

	// The second request must not have cancelled the first: the first
	// operation is still live and neither read has returned.
	if probeDoneClosed(firstStarted.Done) {
		t.Fatal("the second read cancelled the first read's operation")
	}
	requireNoReadOutcome(t, firstDone, "first read")
	requireNoReadOutcome(t, secondDone, "second read")

	// The throttled swarm delivers the missing pieces and both reads finish on
	// their own, still sharing the one loader that neither cancelled.
	firstOutcome := waitReadOutcomeWithin(t, firstDone, "first read", overlapReadTimeout)
	if firstOutcome.err != nil {
		t.Fatalf("first read: %v", firstOutcome.err)
	}
	if want := content[firstOffset+64 : firstOffset+64+64]; !bytes.Equal(firstOutcome.data, want) {
		t.Fatal("first read data differs from the source")
	}
	secondOutcome := waitReadOutcomeWithin(t, secondDone, "second read", overlapReadTimeout)
	if secondOutcome.err != nil {
		t.Fatalf("second read: %v", secondOutcome.err)
	}
	if want := content[secondOffset+64 : secondOffset+64+64]; !bytes.Equal(secondOutcome.data, want) {
		t.Fatal("second read data differs from the source")
	}

	// Playback ended: from here on, shutdown cancellations are expected and
	// are counted separately.
	if got := logState.cancellationCount(logPhasePlayback); got != 0 {
		t.Fatalf("playback produced %d anacrolix reader cancellation logs, want 0", got)
	}
	logState.phase.Store(logPhaseClosing)
}
