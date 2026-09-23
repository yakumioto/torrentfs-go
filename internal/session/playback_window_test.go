package session

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/cache"
)

func playbackTestFile(fileSize, pieceLength int64, store *cache.Cache, coordinator *prefetchCoordinator) *raFile {
	return &raFile{
		cache:       store,
		coordinator: coordinator,
		torrentKey:  "playback-test",
		fileSize:    fileSize,
		pieceLength: pieceLength,
		torrentSize: fileSize,
	}
}

func playbackReadPlan(file *raFile, offset, length int64) (cache.ReadRequest, cache.ReadPlan) {
	request := cache.ReadRequest{
		FileOffset:    offset,
		Length:        length,
		FileStart:     file.fileOffset,
		FileSize:      file.fileSize,
		PieceLength:   file.pieceLength,
		TorrentLength: file.torrentSize,
	}
	return request, cache.Plan(request)
}

func TestPlaybackWindowClassifiesForegroundReads(t *testing.T) {
	const (
		pieceLength = int64(1 << 20)
		fileSize    = int64(256 << 20)
	)
	store := cache.New(512 << 20)
	coordinator := newTestCoordinator(store)
	file := playbackTestFile(fileSize, pieceLength, store, coordinator)
	coordinator.hasAnchor = true
	coordinator.generation = 1
	coordinator.anchor = prefetchAnchor{
		file:        file,
		fileSize:    fileSize,
		pieceLength: pieceLength,
		torrentSize: fileSize,
	}

	cases := []struct {
		name   string
		offset int64
		want   bool
	}{
		{name: "same position", offset: 0, want: false},
		{name: "two megabytes", offset: 2 << 20, want: false},
		{name: "thirty megabytes", offset: 30 << 20, want: false},
		{name: "last byte in window", offset: defaultPlaybackWindow - 1, want: false},
		{name: "exact window end", offset: defaultPlaybackWindow, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			coordinator.mu.Lock()
			got := coordinator.shouldAdvanceGenerationLocked(file, tc.offset)
			coordinator.mu.Unlock()
			if got != tc.want {
				t.Fatalf("shouldAdvanceGenerationLocked(%d) = %v, want %v", tc.offset, got, tc.want)
			}
		})
	}

	// A request may cross the nominal end as long as its first byte is inside.
	request, plan := playbackReadPlan(file, defaultPlaybackWindow-1, 2)
	start, end := foregroundRequestRange(request, plan)
	if start != defaultPlaybackWindow-1 || end != defaultPlaybackWindow+1 {
		t.Fatalf("foregroundRequestRange = [%d, %d), want [%d, %d)", start, end, defaultPlaybackWindow-1, defaultPlaybackWindow+1)
	}
	coordinator.mu.Lock()
	if coordinator.shouldAdvanceGenerationLocked(file, start) {
		coordinator.mu.Unlock()
		t.Fatal("a read starting inside the window was classified as a seek")
	}
	coordinator.mu.Unlock()

	// An active Piece remains in the same generation even at the half-open end.
	coordinator.foregroundPieces[int(defaultPlaybackWindow/pieceLength)] = 1
	coordinator.mu.Lock()
	if coordinator.shouldAdvanceGenerationLocked(file, defaultPlaybackWindow) {
		coordinator.mu.Unlock()
		t.Fatal("a read in an active Piece was classified as a seek")
	}
	coordinator.mu.Unlock()
}

func TestPlaybackWindowSparseReadsDoNotChaseAnchor(t *testing.T) {
	const (
		pieceLength = int64(256 << 10)
		fileSize    = int64(256 << 20)
	)
	store := cache.New(512 << 20)
	coordinator := newTestCoordinator(store)
	file := playbackTestFile(fileSize, pieceLength, store, coordinator)

	initialRequest, initialPlan := playbackReadPlan(file, 0, 64)
	initial := coordinator.beginForeground(file, initialRequest, initialPlan, context.Background())
	initial.finish(0, 64, nil)
	baseline := coordinator.snapshot()
	if baseline.Generation != 1 || baseline.Cursor != 64 {
		t.Fatalf("initial state = generation %d cursor %d; want 1 and 64", baseline.Generation, baseline.Cursor)
	}

	for _, offset := range []int64{2 << 20, 50 << 20, (87 << 20) + (512 << 10), 120 << 20} {
		request, plan := playbackReadPlan(file, offset, 64)
		ticket := coordinator.beginForeground(file, request, plan, context.Background())
		if ticket == nil {
			t.Fatalf("sparse read at %d returned a nil ticket", offset)
		}
		ticket.finish(offset, 64, nil)
		snapshot := coordinator.snapshot()
		if snapshot.Generation != baseline.Generation || snapshot.Cursor != baseline.Cursor {
			t.Fatalf("sparse read at %d moved generation/cursor to %d/%d, want %d/%d", offset, snapshot.Generation, snapshot.Cursor, baseline.Generation, baseline.Cursor)
		}
	}
	if snapshot := coordinator.snapshot(); !snapshot.CandidatePresent {
		t.Fatal("the final out-of-window probe did not remain foreground-only as a candidate")
	}
}

func TestPlaybackWindowCrossFileCandidateConfirmsAndCleansOldWindow(t *testing.T) {
	const (
		pieceLength = int64(1 << 20)
		fileSize    = int64(128 << 20)
		torrentSize = 2 * fileSize
	)
	store := cache.New(512 << 20)
	coordinator := newTestCoordinator(store)
	oldFile := playbackTestFile(fileSize, pieceLength, store, coordinator)
	oldFile.torrentSize = torrentSize
	newFile := playbackTestFile(fileSize, pieceLength, store, coordinator)
	newFile.fileOffset = fileSize
	newFile.torrentSize = torrentSize

	initialRequest, initialPlan := playbackReadPlan(oldFile, 0, 64)
	initial := coordinator.beginForeground(oldFile, initialRequest, initialPlan, context.Background())
	initial.finish(0, 64, nil)

	coordinator.mu.Lock()
	if !coordinator.retainLocked(0) {
		coordinator.mu.Unlock()
		t.Fatal("could not retain the old window piece")
	}
	coordinator.windowPins[0] = struct{}{}
	if !coordinator.budget.tryAcquire() {
		coordinator.mu.Unlock()
		t.Fatal("could not reserve the old window lease")
	}
	coordinator.active[0] = struct{}{}
	coordinator.mu.Unlock()

	firstRequest, firstPlan := playbackReadPlan(newFile, 64<<20, 64)
	first := coordinator.beginForeground(newFile, firstRequest, firstPlan, context.Background())
	first.finish(firstRequest.FileOffset, 64, nil)
	before := coordinator.snapshot()
	if before.Generation != 1 || before.CandidateReads != 1 || before.CandidateStart != 64<<20 {
		t.Fatalf("cross-file candidate before confirmation = generation %d reads %d start %d; want 1, 1, %d", before.Generation, before.CandidateReads, before.CandidateStart, 64<<20)
	}

	secondRequest, secondPlan := playbackReadPlan(newFile, (64<<20)+64, 64)
	second := coordinator.beginForeground(newFile, secondRequest, secondPlan, context.Background())
	second.finish(secondRequest.FileOffset, 64, nil)
	after := coordinator.snapshot()
	if after.Generation != 2 {
		t.Fatalf("cross-file generation = %d, want 2", after.Generation)
	}
	if after.CandidatePresent {
		t.Fatal("cross-file candidate remained after confirmation")
	}
	if after.Cursor != (64<<20)+128 {
		t.Fatalf("cross-file anchor cursor = %d, want %d", after.Cursor, (64<<20)+128)
	}
	if after.ActivePieces != 0 || after.PinnedPieces != 0 {
		t.Fatalf("old resources after cross-file confirmation = active %d pins %d, want 0, 0", after.ActivePieces, after.PinnedPieces)
	}
	if _, used := coordinator.budget.snapshot(); used != 0 {
		t.Fatalf("background budget after cross-file confirmation = %d, want 0", used)
	}

	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.anchor.file != newFile {
		t.Fatal("cross-file confirmation did not install the candidate file as anchor")
	}
	if got := playbackWindowEnd(coordinator.anchor.cursor, coordinator.anchor.fileSize); got != (64<<20)+128+defaultPlaybackWindow {
		t.Fatalf("new cross-file playback window ends at %d, want %d", got, (64<<20)+128+defaultPlaybackWindow)
	}
	desired := coordinator.desiredPiecesLocked()
	if len(desired) == 0 || desired[0] != 192 {
		t.Fatalf("new cross-file desired window starts at %v, want piece 192", desired)
	}
}

func TestPlaybackWindowCandidateRequiresUniqueSecondRead(t *testing.T) {
	const (
		pieceLength = int64(1 << 20)
		fileSize    = int64(256 << 20)
	)
	store := cache.New(512 << 20)
	coordinator := newTestCoordinator(store)
	file := playbackTestFile(fileSize, pieceLength, store, coordinator)

	initialRequest, initialPlan := playbackReadPlan(file, 0, 64)
	initial := coordinator.beginForeground(file, initialRequest, initialPlan, context.Background())
	initial.finish(0, 64, nil)

	firstRequest, firstPlan := playbackReadPlan(file, 100<<20, 64)
	first := coordinator.beginForeground(file, firstRequest, firstPlan, context.Background())
	first.finish(firstRequest.FileOffset, 64, nil)
	duplicateRequest, duplicatePlan := playbackReadPlan(file, 100<<20, 64)
	duplicate := coordinator.beginForeground(file, duplicateRequest, duplicatePlan, context.Background())
	duplicate.finish(duplicateRequest.FileOffset, 64, nil)
	before := coordinator.snapshot()
	if before.Generation != 1 || before.CandidateReads != 1 || before.CandidateUniqueBytes != 64 {
		t.Fatalf("duplicate candidate evidence = generation %d reads %d bytes %d; want 1, 1, 64", before.Generation, before.CandidateReads, before.CandidateUniqueBytes)
	}

	secondRequest, secondPlan := playbackReadPlan(file, (100<<20)+64, 64)
	second := coordinator.beginForeground(file, secondRequest, secondPlan, context.Background())
	second.finish(secondRequest.FileOffset, 64, nil)
	after := coordinator.snapshot()
	if after.Generation != 2 {
		t.Fatalf("generation after unique local read = %d, want 2", after.Generation)
	}
	if after.Cursor != (100<<20)+128 {
		t.Fatalf("cursor after confirmed local read = %d, want %d", after.Cursor, (100<<20)+128)
	}
	if after.CandidatePresent {
		t.Fatal("candidate remained installed after confirmation")
	}
}

func TestPlaybackWindowKeepsAdjacentForegroundFlights(t *testing.T) {
	const (
		pieceLength = int64(1 << 20)
		fileSize    = int64(256 << 20)
	)
	store := cache.New(512 << 20)
	coordinator := newTestCoordinator(store)
	file := playbackTestFile(fileSize, pieceLength, store, coordinator)
	request1, plan1 := playbackReadPlan(file, 100<<20, 64)
	request2, plan2 := playbackReadPlan(file, 102<<20, 64)
	first := coordinator.beginForeground(file, request1, plan1, context.Background())
	if first == nil {
		t.Fatal("first foreground ticket is nil")
	}
	second := coordinator.beginForeground(file, request2, plan2, context.Background())
	if second == nil {
		t.Fatal("second foreground ticket is nil")
	}
	if second.generation != first.generation {
		t.Fatalf("adjacent tickets used generations %d and %d", first.generation, second.generation)
	}
	select {
	case <-first.ctx.Done():
		t.Fatal("an adjacent foreground read was cancelled")
	default:
	}
	snapshot := coordinator.snapshot()
	if snapshot.ForegroundCancels != 0 {
		t.Fatalf("foreground cancellations = %d, want 0", snapshot.ForegroundCancels)
	}
	if snapshot.ForegroundTickets != 2 {
		t.Fatalf("foreground tickets = %d, want 2", snapshot.ForegroundTickets)
	}
	first.finish(request1.FileOffset, 64, nil)
	second.finish(request2.FileOffset, 64, nil)
	if got := coordinator.snapshot().ForegroundTickets; got != 0 {
		t.Fatalf("foreground tickets after finish = %d, want 0", got)
	}
}

func TestPlaybackWindowSeekCancelsOnlyStaleForegroundFlights(t *testing.T) {
	const (
		pieceLength = int64(1 << 20)
		fileSize    = int64(256 << 20)
	)
	store := cache.New(512 << 20)
	coordinator := newTestCoordinator(store)
	file := playbackTestFile(fileSize, pieceLength, store, coordinator)
	oldRequest, oldPlan := playbackReadPlan(file, 0, 64)
	old := coordinator.beginForeground(file, oldRequest, oldPlan, context.Background())
	if old == nil {
		t.Fatal("old foreground ticket is nil")
	}
	firstRequest, firstPlan := playbackReadPlan(file, 128<<20, 64)
	firstCandidate := coordinator.beginForeground(file, firstRequest, firstPlan, context.Background())
	if firstCandidate == nil {
		t.Fatal("first candidate ticket is nil")
	}
	select {
	case <-old.ctx.Done():
		t.Fatal("a single far probe cancelled the stale foreground ticket")
	default:
	}

	secondRequest, secondPlan := playbackReadPlan(file, (128<<20)+64, 64)
	secondCandidate := coordinator.beginForeground(file, secondRequest, secondPlan, context.Background())
	if secondCandidate == nil {
		t.Fatal("second candidate ticket is nil")
	}
	select {
	case <-secondCandidate.ctx.Done():
		t.Fatal("candidate confirmation cancelled the latest foreground ticket")
	default:
	}
	secondCandidate.startPiece(secondPlan.Spans[0].Index)
	snapshot := coordinator.snapshot()
	if snapshot.Generation != 1 || !snapshot.CandidatePresent {
		t.Fatalf("candidate state before completion = generation %d, present %v; want generation 1 and candidate", snapshot.Generation, snapshot.CandidatePresent)
	}
	if snapshot.ForegroundTickets != 3 || snapshot.ForegroundPieces != 1 {
		t.Fatalf("foreground state before completion = tickets %d, pieces %d; want 3, 1", snapshot.ForegroundTickets, snapshot.ForegroundPieces)
	}
	firstCandidate.finish(firstRequest.FileOffset, 64, nil)
	secondCandidate.finish(secondRequest.FileOffset, 64, nil)
	select {
	case <-old.ctx.Done():
	default:
		t.Fatal("confirmed seek did not cancel the stale foreground ticket")
	}
	snapshot = coordinator.snapshot()
	if snapshot.Generation != 2 {
		t.Fatalf("generation = %d after confirmed seek, want 2", snapshot.Generation)
	}
	if snapshot.ForegroundTickets != 0 {
		t.Fatalf("foreground tickets after confirmed seek = %d, want 0", snapshot.ForegroundTickets)
	}
	if snapshot.ForegroundCancels != 1 {
		t.Fatalf("foreground cancellations = %d, want 1", snapshot.ForegroundCancels)
	}
	secondCandidate.finishPiece(secondPlan.Spans[0].Index)
}

func TestPieceFetcherSharedFlightSurvivesOneWaiterCancellation(t *testing.T) {
	rootContext, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	var fetcher *pieceFetcher
	fetcher = &pieceFetcher{
		pieceLength: 4,
		torrentSize: 4,
		rootContext: rootContext,
		rootCancel:  rootCancel,
		inflight:    make(map[int]*pieceFlight),
		flights:     make(map[*pieceFlight]struct{}),
		runFlightFn: func(flight *pieceFlight, _, _ int64) {
			startOnce.Do(func() { close(started) })
			<-release
			fetcher.mu.Lock()
			flight.data = []byte("data")
			flight.finished = true
			delete(fetcher.inflight, flight.index)
			delete(fetcher.flights, flight)
			close(flight.done)
			fetcher.mu.Unlock()
			flight.cancel()
		},
	}

	firstContext, firstCancel := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() {
		_, err := fetcher.ReadAtContext(firstContext, make([]byte, 4), 0, 0)
		firstResult <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shared flight did not start")
	}

	secondResult := make(chan []byte, 1)
	secondErr := make(chan error, 1)
	go func() {
		data := make([]byte, 4)
		_, err := fetcher.ReadAtContext(context.Background(), data, 0, 0)
		secondResult <- data
		secondErr <- err
	}()

	var flight *pieceFlight
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		fetcher.mu.Lock()
		flight = fetcher.inflight[0]
		waiters := 0
		if flight != nil {
			waiters = flight.waiters
		}
		fetcher.mu.Unlock()
		if waiters == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if flight == nil || flight.waiters != 2 {
		t.Fatal("second waiter did not join the existing flight")
	}
	firstCancel()
	select {
	case err := <-firstResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter did not return")
	}
	select {
	case <-flight.ctx.Done():
		t.Fatal("cancelling one waiter cancelled the shared flight")
	default:
	}
	close(release)
	select {
	case err := <-secondErr:
		if err != nil {
			t.Fatalf("remaining waiter error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("remaining waiter did not return")
	}
	select {
	case data := <-secondResult:
		if !bytes.Equal(data, []byte("data")) {
			t.Fatalf("remaining waiter data = %q, want %q", data, "data")
		}
	case <-time.After(time.Second):
		t.Fatal("remaining waiter data was not delivered")
	}
}

type blockingPlaybackSource struct {
	calls    chan int64
	started  chan struct{}
	startOne sync.Once
}

func (s *blockingPlaybackSource) ReadAtContext(ctx context.Context, dst []byte, off, _ int64) (int, error) {
	s.calls <- off
	if off == 0 {
		s.startOne.Do(func() { close(s.started) })
		<-ctx.Done()
		return 0, ctx.Err()
	}
	for i := range dst {
		dst[i] = byte(off + int64(i))
	}
	return len(dst), nil
}

func (s *blockingPlaybackSource) Close() error { return nil }

func TestPlaybackWindowStaleCrossPieceReadStopsBeforeNextSpan(t *testing.T) {
	const (
		pieceLength = int64(1 << 20)
		fileSize    = int64(256 << 20)
	)
	store := cache.New(512 << 20)
	coordinator := newTestCoordinator(store)
	source := &blockingPlaybackSource{calls: make(chan int64, 8), started: make(chan struct{})}
	file := playbackTestFile(fileSize, pieceLength, store, coordinator)
	file.loader = source

	oldDone := make(chan error, 1)
	go func() {
		_, err := file.ReadAtContext(context.Background(), make([]byte, 2*pieceLength), 0)
		oldDone <- err
	}()
	select {
	case <-source.started:
	case <-time.After(time.Second):
		t.Fatal("old multi-piece read did not start")
	}

	store.Put(cache.Key{Torrent: file.torrentKey, Piece: 128}, bytes.Repeat([]byte{7}, int(pieceLength)))
	firstCandidate := make([]byte, 64)
	if n, err := file.ReadAtContext(context.Background(), firstCandidate, 128*pieceLength); err != nil || n != len(firstCandidate) {
		t.Fatalf("first candidate cache-hit read = %d, %v; want %d, nil", n, err, len(firstCandidate))
	}
	secondCandidate := make([]byte, 64)
	if n, err := file.ReadAtContext(context.Background(), secondCandidate, 128*pieceLength+64); err != nil || n != len(secondCandidate) {
		t.Fatalf("second candidate cache-hit read = %d, %v; want %d, nil", n, err, len(secondCandidate))
	}
	select {
	case err := <-oldDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stale multi-piece read error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("confirmed seek did not stop the stale multi-piece read")
	}

	closeCalls := false
	for {
		select {
		case off := <-source.calls:
			if off == pieceLength {
				t.Fatal("stale read started the next Piece after Seek")
			}
		default:
			closeCalls = true
		}
		if closeCalls {
			break
		}
	}
	if got := coordinator.snapshot().StaleSpanRejects; got == 0 {
		t.Fatal("stale span rejection was not recorded")
	}
}

func TestPlaybackWindowToleratesHighFirstAdjacentRead(t *testing.T) {
	const (
		pieceLength = int64(1 << 20)
		fileSize    = int64(256 << 20)
	)
	store := cache.New(512 << 20)
	coordinator := newTestCoordinator(store)
	file := playbackTestFile(fileSize, pieceLength, store, coordinator)
	coordinator.hasAnchor = true
	coordinator.generation = 1
	coordinator.anchor = prefetchAnchor{
		file:            file,
		fileSize:        fileSize,
		pieceLength:     pieceLength,
		torrentSize:     fileSize,
		cursor:          102 << 20,
		committedCursor: 100 << 20,
	}
	ticket := &foregroundTicket{
		coordinator:  coordinator,
		id:           1,
		generation:   1,
		file:         file,
		requestStart: 102 << 20,
		active:       map[int]int{102: 1},
	}
	coordinator.foreground[1] = ticket
	coordinator.foregroundPieces[102] = 1
	coordinator.mu.Lock()
	got := coordinator.shouldAdvanceGenerationLocked(file, 101<<20)
	coordinator.mu.Unlock()
	if got {
		t.Fatal("a lower adjacent read arrived after a higher one and was classified as a seek")
	}
}

func TestPlaybackWindowFailedColdStartDoesNotConfirmAnchor(t *testing.T) {
	const (
		pieceLength = int64(1 << 20)
		fileSize    = int64(256 << 20)
	)
	store := cache.New(512 << 20)
	coordinator := newTestCoordinator(store)
	file := playbackTestFile(fileSize, pieceLength, store, coordinator)
	request, plan := playbackReadPlan(file, 8<<20, 64)
	ticket := coordinator.beginForeground(file, request, plan, context.Background())
	if ticket == nil {
		t.Fatal("cold-start ticket is nil")
	}
	ticket.finish(request.FileOffset, 0, context.Canceled)
	snapshot := coordinator.snapshot()
	if snapshot.AnchorConfirmed || snapshot.PlaybackAnchor != 0 || snapshot.PinnedPieces != 0 || snapshot.CandidatePresent {
		t.Fatalf("failed cold start state = confirmed %v anchor %d pins %d candidate %v", snapshot.AnchorConfirmed, snapshot.PlaybackAnchor, snapshot.PinnedPieces, snapshot.CandidatePresent)
	}

	nextRequest, nextPlan := playbackReadPlan(file, 16<<20, 64)
	next := coordinator.beginForeground(file, nextRequest, nextPlan, context.Background())
	if next == nil || next.generation != 2 {
		t.Fatalf("next cold-start generation = %v, want 2", next)
	}
	next.finish(nextRequest.FileOffset, 64, nil)
}

func TestPlaybackWindowFailedAdmissionDoesNotMoveCursor(t *testing.T) {
	const (
		pieceLength = int64(1 << 20)
		fileSize    = int64(256 << 20)
	)
	store := cache.New(512 << 20)
	coordinator := newTestCoordinator(store)
	file := playbackTestFile(fileSize, pieceLength, store, coordinator)
	coordinator.hasAnchor = true
	coordinator.generation = 1
	coordinator.anchor = prefetchAnchor{
		file:        file,
		fileSize:    fileSize,
		pieceLength: pieceLength,
		torrentSize: fileSize,
	}
	request, plan := playbackReadPlan(file, 32<<20, 64)
	ticket := coordinator.beginForeground(file, request, plan, context.Background())
	if ticket == nil {
		t.Fatal("foreground ticket is nil")
	}
	ticket.finish(request.FileOffset, 0, context.Canceled)
	snapshot := coordinator.snapshot()
	if snapshot.Cursor != 0 {
		t.Fatalf("cursor after failed admission = %d, want 0", snapshot.Cursor)
	}
	coordinator.mu.Lock()
	seek := coordinator.shouldAdvanceGenerationLocked(file, 0)
	coordinator.mu.Unlock()
	if seek {
		t.Fatal("retry at the original position was classified as a seek")
	}
}

func TestPlaybackWindowReleaseFileDropsRetainedOldTicket(t *testing.T) {
	const (
		pieceLength = int64(1 << 20)
		fileSize    = int64(256 << 20)
	)
	store := cache.New(512 << 20)
	coordinator := newTestCoordinator(store)
	oldFile := playbackTestFile(fileSize, pieceLength, store, coordinator)
	latestFile := playbackTestFile(fileSize, pieceLength, store, coordinator)
	coordinator.hasAnchor = true
	coordinator.anchor = prefetchAnchor{file: latestFile, fileSize: fileSize, pieceLength: pieceLength, torrentSize: fileSize}
	if !coordinator.retainLocked(0) {
		t.Fatal("could not reserve the retained Piece")
	}
	ticket := &foregroundTicket{
		coordinator: coordinator,
		id:          1,
		generation:  1,
		file:        oldFile,
		wanted:      []int{0},
		pinned:      []int{0},
	}
	coordinator.foreground[1] = ticket
	coordinator.releaseFile(oldFile)
	snapshot := coordinator.snapshot()
	if snapshot.ForegroundTickets != 0 || snapshot.PinnedPieces != 0 {
		t.Fatalf("old file resources after release = tickets %d, pins %d; want 0, 0", snapshot.ForegroundTickets, snapshot.PinnedPieces)
	}
}

func TestPlaybackWindowForegroundStartReleasesNormalLease(t *testing.T) {
	store := cache.New(1 << 20)
	coordinator := newTestCoordinator(store)
	file := playbackTestFile(1<<20, 1<<20, store, coordinator)
	if !coordinator.budget.tryAcquire() {
		t.Fatal("could not reserve the test Normal token")
	}
	coordinator.active[0] = struct{}{}
	ticket := &foregroundTicket{
		coordinator: coordinator,
		id:          1,
		generation:  1,
		file:        file,
		active:      make(map[int]int),
	}
	coordinator.foreground[1] = ticket
	ticket.startPiece(0)
	if snapshot := coordinator.snapshot(); snapshot.ActivePieces != 0 || snapshot.ForegroundPieces != 1 {
		t.Fatalf("foreground start state = active %d, foreground %d; want 0, 1", snapshot.ActivePieces, snapshot.ForegroundPieces)
	}
	if _, used := coordinator.budget.snapshot(); used != 0 {
		t.Fatalf("Normal budget after foreground start = %d, want 0", used)
	}
	ticket.finishPiece(0)
}

func TestPlaybackWindowBackwardSeekWhileHigherReadOutstanding(t *testing.T) {
	const (
		pieceLength = int64(1 << 20)
		fileSize    = int64(256 << 20)
	)
	store := cache.New(512 << 20)
	coordinator := newTestCoordinator(store)
	file := playbackTestFile(fileSize, pieceLength, store, coordinator)
	coordinator.hasAnchor = true
	coordinator.generation = 1
	coordinator.anchor = prefetchAnchor{
		file:            file,
		fileSize:        fileSize,
		pieceLength:     pieceLength,
		torrentSize:     fileSize,
		cursor:          120 << 20,
		committedCursor: 120 << 20,
	}
	oldContext, oldCancel := context.WithCancel(context.Background())
	defer oldCancel()
	oldRequest, oldPlan := playbackReadPlan(file, 120<<20, 64)
	old := coordinator.beginForeground(file, oldRequest, oldPlan, oldContext)
	if old == nil {
		t.Fatal("old high foreground ticket is nil")
	}
	old.startPiece(oldPlan.Spans[0].Index)

	firstRequest, firstPlan := playbackReadPlan(file, 10<<20, 64)
	firstCandidate := coordinator.beginForeground(file, firstRequest, firstPlan, context.Background())
	if firstCandidate == nil {
		t.Fatal("first backward candidate ticket is nil")
	}
	secondRequest, secondPlan := playbackReadPlan(file, (10<<20)+64, 64)
	secondCandidate := coordinator.beginForeground(file, secondRequest, secondPlan, context.Background())
	if secondCandidate == nil {
		t.Fatal("second backward candidate ticket is nil")
	}
	if firstCandidate.generation != 1 || secondCandidate.generation != 1 {
		t.Fatalf("backward candidate generations = %d, %d; want both 1", firstCandidate.generation, secondCandidate.generation)
	}
	select {
	case <-old.ctx.Done():
		t.Fatal("a single backward probe cancelled the higher outstanding ticket")
	default:
	}
	firstCandidate.finish(firstRequest.FileOffset, 64, nil)
	secondCandidate.finish(secondRequest.FileOffset, 64, nil)
	select {
	case <-old.ctx.Done():
	default:
		t.Fatal("confirmed backward seek did not cancel the higher outstanding ticket")
	}
	if got := coordinator.snapshot().Generation; got != 2 {
		t.Fatalf("generation after confirmed backward seek = %d, want 2", got)
	}
}
