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
	newRequest, newPlan := playbackReadPlan(file, 128<<20, 64)
	latest := coordinator.beginForeground(file, newRequest, newPlan, context.Background())
	if latest == nil {
		t.Fatal("latest foreground ticket is nil")
	}
	select {
	case <-old.ctx.Done():
	default:
		t.Fatal("a far seek did not cancel the stale foreground ticket")
	}
	select {
	case <-latest.ctx.Done():
		t.Fatal("a far seek cancelled the latest foreground ticket")
	default:
	}
	snapshot := coordinator.snapshot()
	if snapshot.Generation != 2 {
		t.Fatalf("generation = %d after far seek, want 2", snapshot.Generation)
	}
	latest.startPiece(newPlan.Spans[0].Index)
	snapshot = coordinator.snapshot()
	if snapshot.ForegroundTickets != 1 || snapshot.ForegroundPieces != 1 {
		t.Fatalf("latest foreground state = tickets %d, pieces %d; want 1, 1", snapshot.ForegroundTickets, snapshot.ForegroundPieces)
	}
	if snapshot.ForegroundCancels != 1 {
		t.Fatalf("foreground cancellations = %d, want 1", snapshot.ForegroundCancels)
	}
	latest.finish(newRequest.FileOffset, 64, nil)
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
	latest := make([]byte, 64)
	if n, err := file.ReadAtContext(context.Background(), latest, 128*pieceLength); err != nil || n != len(latest) {
		t.Fatalf("latest cache-hit read = %d, %v; want %d, nil", n, err, len(latest))
	}
	select {
	case err := <-oldDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stale multi-piece read error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stale multi-piece read did not stop")
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

	latestRequest, latestPlan := playbackReadPlan(file, 10<<20, 64)
	latest := coordinator.beginForeground(file, latestRequest, latestPlan, context.Background())
	if latest == nil {
		t.Fatal("backward seek ticket is nil")
	}
	if latest.generation != 2 {
		t.Fatalf("backward out-of-window seek stayed in generation %d, want 2", latest.generation)
	}
	select {
	case <-old.ctx.Done():
	default:
		t.Fatal("backward out-of-window seek did not cancel the higher outstanding ticket")
	}
	select {
	case <-latest.ctx.Done():
		t.Fatal("backward seek cancelled the latest ticket")
	default:
	}
	latest.finish(latestRequest.FileOffset, 64, nil)
}
