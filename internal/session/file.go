package session

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent"

	"github.com/yakumioto/torrentfs-go/internal/cache"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

// readProbeEvent reports one step of a loader read to a test-installed probe.
// Kind is "admission-attempt" just before the read waits for the admission
// permit, or "reader-started" once the read is admitted and about to enter the
// underlying reader. Done is the operation context's Done channel, so a test
// can tell whether the operation was cancelled without inferring it from the
// read's result.
type readProbeEvent struct {
	Kind      string
	Offset    int64
	Operation uint64
	Done      <-chan struct{}
}

// readProbe, when set, receives loader read events. It is a test-only seam:
// production never installs one. Sends are non-blocking, so a full or stalled
// probe channel can never hold up a reader.
var readProbe atomic.Pointer[chan readProbeEvent]

func emitReadProbe(event readProbeEvent) {
	probe := readProbe.Load()
	if probe == nil {
		return
	}
	select {
	case (*probe) <- event:
	default:
	}
}

type pieceSource interface {
	ReadAtContext(context.Context, []byte, int64, int64) (int, error)
	Close() error
}

const defaultStreamingReadahead int64 = 8 << 20

// pieceFetcher owns one Reader per missing Piece. A flight is shared by all
// foreground waiters for that Piece, while different Pieces use independent
// Readers and can make progress concurrently.
type pieceFetcher struct {
	torrent     *torrent.Torrent
	cache       *cache.Cache
	torrentKey  string
	pieceLength int64
	torrentSize int64

	mu          sync.Mutex
	rootContext context.Context
	rootCancel  context.CancelFunc
	inflight    map[int]*pieceFlight
	flights     map[*pieceFlight]struct{}
	runFlightFn func(*pieceFlight, int64, int64)
	closed      bool
}

type pieceFlight struct {
	index    int
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	data     []byte
	err      error
	waiters  int
	finished bool
}

func newPieceFetcher(t *torrent.Torrent, c *cache.Cache) *pieceFetcher {
	ctx, cancel := context.WithCancel(context.Background())
	info := t.Info()
	var pieceLength int64
	if info != nil {
		pieceLength = info.PieceLength
	}
	return &pieceFetcher{
		torrent:     t,
		cache:       c,
		torrentKey:  t.InfoHash().HexString(),
		pieceLength: pieceLength,
		torrentSize: t.Length(),
		rootContext: ctx,
		rootCancel:  cancel,
		inflight:    make(map[int]*pieceFlight),
		flights:     make(map[*pieceFlight]struct{}),
	}
}

func (f *pieceFetcher) ReadAtContext(requestCtx context.Context, p []byte, off, _ int64) (int, error) {
	if off < 0 || f.pieceLength <= 0 {
		return 0, filesystem.ErrInvalidName
	}
	if requestCtx == nil {
		requestCtx = context.Background()
	}
	if err := requestCtx.Err(); err != nil {
		return 0, err
	}
	index := int(off / f.pieceLength)
	start := int64(index) * f.pieceLength
	length := f.pieceLength
	if remaining := f.torrentSize - start; remaining < length {
		length = remaining
	}
	if start < 0 || length <= 0 || off >= start+length {
		return 0, io.EOF
	}
	emitReadProbe(readProbeEvent{Kind: "admission-attempt", Offset: start})
	flight, err := f.joinFlight(index, start, length)
	if err != nil {
		return 0, err
	}
	select {
	case <-flight.done:
		f.detachFlight(flight)
	case <-requestCtx.Done():
		f.detachFlight(flight)
		return 0, requestCtx.Err()
	}
	if flight.err != nil {
		return 0, flight.err
	}
	from := off - start
	if from < 0 || from >= int64(len(flight.data)) {
		return 0, io.EOF
	}
	n := copy(p, flight.data[from:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *pieceFetcher) joinFlight(index int, start, length int64) (*pieceFlight, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, filesystem.ErrClosed
	}
	if flight, ok := f.inflight[index]; ok {
		flight.waiters++
		return flight, nil
	}
	ctx, cancel := context.WithCancel(f.rootContext)
	flight := &pieceFlight{
		index:   index,
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
		waiters: 1,
	}
	f.inflight[index] = flight
	if f.flights == nil {
		f.flights = make(map[*pieceFlight]struct{})
	}
	f.flights[flight] = struct{}{}
	runFlight := f.runFlightFn
	if runFlight == nil {
		runFlight = f.runFlight
	}
	go runFlight(flight, start, length)
	return flight, nil
}

func (f *pieceFetcher) runFlight(flight *pieceFlight, start, length int64) {
	data := make([]byte, int(length))
	reader := f.torrent.NewReader()
	reader.SetContext(flight.ctx)
	reader.SetReadahead(0)
	var err error
	if _, err = reader.Seek(start, io.SeekStart); err == nil {
		emitReadProbe(readProbeEvent{Kind: "reader-started", Offset: start, Done: flight.ctx.Done()})
		var n int
		n, err = io.ReadFull(reader, data)
		if err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
		if err == nil && n != len(data) {
			err = io.ErrUnexpectedEOF
		}
	}
	if closeErr := reader.Close(); err == nil && closeErr != nil && !errors.Is(closeErr, filesystem.ErrClosed) {
		err = closeErr
	}
	if err == nil && f.cache != nil {
		f.cache.Put(cache.Key{Torrent: f.torrentKey, Piece: flight.index}, data)
	}
	f.mu.Lock()
	flight.data = data
	flight.err = err
	flight.finished = true
	if current, ok := f.inflight[flight.index]; ok && current == flight {
		delete(f.inflight, flight.index)
	}
	delete(f.flights, flight)
	close(flight.done)
	f.mu.Unlock()
	flight.cancel()
}

func (f *pieceFetcher) detachFlight(flight *pieceFlight) {
	f.mu.Lock()
	if flight.waiters > 0 {
		flight.waiters--
	}
	if flight.waiters == 0 && !flight.finished {
		if current, ok := f.inflight[flight.index]; ok && current == flight {
			delete(f.inflight, flight.index)
		}
		flight.cancel()
	}
	f.mu.Unlock()
}

func (f *pieceFetcher) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	f.rootCancel()
	flights := make([]*pieceFlight, 0, len(f.flights))
	for flight := range f.flights {
		flight.cancel()
		flights = append(flights, flight)
	}
	f.mu.Unlock()
	for _, flight := range flights {
		<-flight.done
	}
	return nil
}

// pieceLoader serializes access to one whole-torrent anacrolix reader.
type pieceLoader struct {
	mu sync.Mutex
	// admission is a capacity-1 permit: exactly one operation at a time drives
	// the shared Reader. A waiter cancelled while queued takes the ctx.Done()
	// branch and leaves without a permit, so cancellation interrupts the wait
	// without ever touching another request's read.
	//
	// Must be non-nil: build the loader through newPieceLoader (or newAdmission).
	admission chan struct{}

	operationMu     sync.Mutex
	rootContext     context.Context
	rootCancel      context.CancelFunc
	activeCancel    context.CancelFunc
	nextOperation   uint64
	activeOperation uint64

	r      torrent.Reader
	closed bool
	err    error
}

var _ pieceSource = (*pieceLoader)(nil)

func newPieceLoader(t *torrent.Torrent) *pieceLoader {
	ctx, cancel := context.WithCancel(context.Background())
	r := t.NewReader()
	r.SetContext(ctx)
	return &pieceLoader{
		admission:   newAdmission(),
		r:           r,
		rootContext: ctx,
		rootCancel:  cancel,
	}
}

// newAdmission returns the capacity-1 permit channel, already holding its
// single permit.
func newAdmission() chan struct{} {
	admission := make(chan struct{}, 1)
	admission <- struct{}{}
	return admission
}

// acquireAdmission takes the single admission permit. It returns the matching
// release function, or ctx.Err() if ctx ended first — in which case no permit
// is held and the caller must not release. The wait is interruptible by ctx
// only: it never cancels the read in flight.
func (l *pieceLoader) acquireAdmission(ctx context.Context) (func(), error) {
	select {
	case <-l.admission:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return l.releaseAdmission, nil
}

// releaseAdmission returns the permit. Every acquirer holds it exactly once and
// returns it through a single deferred call, so the channel is empty whenever
// the permit is held and this send never blocks.
func (l *pieceLoader) releaseAdmission() { l.admission <- struct{}{} }

func (l *pieceLoader) cancelActive() {
	l.operationMu.Lock()
	cancel := l.activeCancel
	l.operationMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (l *pieceLoader) beginOperation() (context.Context, uint64, context.CancelFunc) {
	l.operationMu.Lock()
	defer l.operationMu.Unlock()
	ctx, cancel := context.WithCancel(l.rootContext)
	l.nextOperation++
	operation := l.nextOperation
	l.activeOperation = operation
	l.activeCancel = cancel
	return ctx, operation, cancel
}

func (l *pieceLoader) endOperation(operation uint64, cancel context.CancelFunc) {
	l.operationMu.Lock()
	if l.activeOperation == operation {
		l.activeOperation = 0
		l.activeCancel = nil
	}
	l.operationMu.Unlock()
	cancel()
}

// ReadAtContext reads off bytes at off into p on behalf of requestCtx. Reads
// are serialized: only one operation ever drives the shared anacrolix reader,
// and a new read waits for the one in flight instead of cancelling it.
//
// requestCtx ends an operation that has already started and nothing else. Once
// this read is admitted, only its own operation context (requestCtx, the
// loader's root context, or Close) can end it, and requestCtx never cancels
// another request's in-flight read. While this read is still queued, cancelling
// requestCtx ends that wait immediately: the read leaves the queue without the
// admission permit and returns its context's error. Close still drains the
// queue by cancelling the read in flight rather than by interrupting waiters.
func (l *pieceLoader) ReadAtContext(requestCtx context.Context, p []byte, off, readahead int64) (int, error) {
	if off < 0 {
		return 0, filesystem.ErrInvalidName
	}
	if requestCtx == nil {
		requestCtx = context.Background()
	}
	if err := requestCtx.Err(); err != nil {
		return 0, err
	}
	emitReadProbe(readProbeEvent{Kind: "admission-attempt", Offset: off})
	// The wait is interruptible by this request's own context only: a waiter
	// that takes the ctx.Done() branch leaves the queue without a permit and
	// without touching the shared reader, so a queued read can never cancel the
	// read in flight. A read cancelled after it is admitted returns at the
	// recheck below, releasing the permit it just took. Close drains the queue
	// by cancelling the read in flight, not by interrupting the waiters.
	release, err := l.acquireAdmission(requestCtx)
	if err != nil {
		return 0, err
	}
	defer release()
	if err := requestCtx.Err(); err != nil {
		return 0, err
	}
	ctx, operation, cancel := l.beginOperation()
	defer l.endOperation(operation, cancel)
	// Connect this request to its own operation only: the callback cancels the
	// operation when the request goes away, and is stopped before the
	// operation ends so the callback cannot outlive it.
	stopRequestCancel := context.AfterFunc(requestCtx, cancel)
	defer stopRequestCancel()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, filesystem.ErrClosed
	}
	l.r.SetReadahead(readahead)
	l.r.SetContext(ctx)
	if _, err := l.r.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	emitReadProbe(readProbeEvent{Kind: "reader-started", Offset: off, Operation: operation, Done: ctx.Done()})
	n, err := io.ReadFull(l.r, p)
	if err == io.ErrUnexpectedEOF {
		err = io.EOF
	}
	return n, err
}

func (l *pieceLoader) Close() error {
	l.rootCancel()
	l.cancelActive()
	// Close waits for the in-flight read without a context: it is what makes
	// that read unwind, so it must not be interruptible. Queue drain is
	// unchanged — Close takes the permit, marks the loader closed, and every
	// waiter admitted afterwards returns ErrClosed (or its own context error).
	<-l.admission
	defer l.releaseAdmission()
	l.cancelActive()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return l.err
	}
	l.closed = true
	l.err = l.r.Close()
	if l.err != nil && errors.Is(l.err, filesystem.ErrClosed) {
		return nil
	}
	return l.err
}

// raFile maps a file-local ReaderAt request onto cached whole-torrent pieces.
type raFile struct {
	mu sync.RWMutex

	loader      pieceSource
	cache       *cache.Cache
	coordinator *prefetchCoordinator
	torrentKey  string
	fileOffset  int64
	fileSize    int64
	pieceLength int64
	torrentSize int64
	readahead   int64
	closed      bool
	handles     int
	// window holds the pieces pinned by the most recent read: the requested
	// range plus its readahead. Replacing it lets the previous region fall back
	// to the LRU tail, so a seek keeps the new region and lets the old one go.
	window []cache.Key
}

var _ io.ReaderAt = (*raFile)(nil)
var _ io.Closer = (*raFile)(nil)

type openedFile struct {
	file *raFile
	once sync.Once
}

var _ io.ReaderAt = (*openedFile)(nil)
var _ io.Closer = (*openedFile)(nil)

func (f *openedFile) ReadAt(p []byte, off int64) (int, error) {
	return f.file.ReadAt(p, off)
}

func (f *openedFile) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	return f.file.ReadAtContext(ctx, p, off)
}

func (f *openedFile) Close() error {
	f.once.Do(func() { f.file.releaseHandle() })
	return nil
}

func (f *raFile) acquireHandle() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.handles++
	return true
}

func (f *raFile) releaseHandle() {
	f.mu.Lock()
	if f.handles > 0 {
		f.handles--
	}
	last := f.handles == 0
	coordinator := f.coordinator
	f.mu.Unlock()
	if last && coordinator != nil {
		coordinator.releaseFile(f)
	}
}

func (f *raFile) ReadAt(p []byte, off int64) (int, error) {
	return f.ReadAtContext(context.Background(), p, off)
}

// ReadAtContext serves one file-local read on behalf of ctx. Cancelling ctx
// ends this read only; a true playback-window seek may also end stale reads from
// an older generation.
func (f *raFile) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, filesystem.ErrInvalidName
	}
	if len(p) == 0 {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	f.mu.RLock()
	if f.closed {
		f.mu.RUnlock()
		return 0, filesystem.ErrClosed
	}
	loader := f.loader
	coordinator := f.coordinator
	request := cache.ReadRequest{
		FileOffset:    off,
		Length:        int64(len(p)),
		FileStart:     f.fileOffset,
		FileSize:      f.fileSize,
		PieceLength:   f.pieceLength,
		TorrentLength: f.torrentSize,
	}
	f.mu.RUnlock()

	if err := ctx.Err(); err != nil {
		return 0, err
	}
	plan := cache.Plan(request)
	if len(plan.Spans) == 0 {
		return 0, io.EOF
	}
	var ticket *foregroundTicket
	readCtx := ctx
	if coordinator != nil {
		ticket = coordinator.beginForeground(f, request, plan, ctx)
		if ticket != nil && ticket.ctx != nil {
			readCtx = ticket.ctx
		}
	} else {
		f.protectWindow(f.windowKeys(request, int64(len(p))+f.readaheadBytes()))
	}

	written := 0
	finish := func(err error) (int, error) {
		if ticket != nil {
			ticket.finish(off, written, err)
		}
		return written, err
	}
	for _, span := range plan.Spans {
		if err := readCtx.Err(); err != nil {
			if ticket != nil {
				ticket.recordStaleSpan()
			}
			return finish(err)
		}
		pieceMiss := false
		if ticket != nil {
			_, cached := f.cache.Get(cache.Key{Torrent: f.torrentKey, Piece: span.Index})
			pieceMiss = !cached
			if pieceMiss {
				ticket.startPiece(span.Index)
			}
		}
		data, err := f.span(readCtx, loader, span)
		if pieceMiss {
			ticket.finishPiece(span.Index)
		}
		if err != nil {
			if ticket != nil && readCtx.Err() != nil {
				ticket.recordStaleSpan()
			}
			return finish(err)
		}
		written += copy(p[written:], data)
	}
	if written < len(p) {
		return finish(io.EOF)
	}
	return finish(nil)
}

func (f *raFile) readaheadBytes() int64 {
	f.mu.RLock()
	readahead := f.readahead
	f.mu.RUnlock()
	if readahead <= 0 {
		return defaultStreamingReadahead
	}
	return readahead
}

func (f *raFile) span(ctx context.Context, loader pieceSource, pieceSpan cache.PieceSpan) ([]byte, error) {
	end := pieceSpan.Offset + pieceSpan.Length
	if pieceSpan.Offset < 0 || pieceSpan.Length <= 0 || end < pieceSpan.Offset {
		return nil, io.ErrUnexpectedEOF
	}
	key := cache.Key{Torrent: f.torrentKey, Piece: pieceSpan.Index}
	if value, ok := f.cache.Get(key); ok {
		if end > int64(len(value)) {
			return nil, io.ErrUnexpectedEOF
		}
		return value[pieceSpan.Offset:end], nil
	}

	data, err := f.piece(ctx, loader, pieceSpan.Index)
	if err != nil {
		return nil, err
	}
	if end > int64(len(data)) {
		return nil, io.ErrUnexpectedEOF
	}
	return data[pieceSpan.Offset:end], nil
}

func (f *raFile) piece(ctx context.Context, loader pieceSource, index int) ([]byte, error) {
	key := cache.Key{Torrent: f.torrentKey, Piece: index}
	if value, ok := f.cache.Get(key); ok {
		return value, nil
	}

	start := int64(index) * f.pieceLength
	length := f.pieceLength
	if remaining := f.torrentSize - start; remaining < length {
		length = remaining
	}
	if start < 0 || length <= 0 {
		return nil, io.EOF
	}
	data := make([]byte, int(length))
	if err := f.readPiece(ctx, loader, data, start); err != nil {
		return nil, err
	}
	f.mu.RLock()
	if f.closed {
		f.mu.RUnlock()
		return nil, filesystem.ErrClosed
	}
	f.cache.Put(key, data)
	f.mu.RUnlock()
	return data, nil
}

// readPiece fills data from loader at the torrent-global offset start.
//
// A reader that already had the piece can answer the next read with EOF once an
// eviction took it away: the data is not absent, it has to be fetched again. The
// attempt is repeated a bounded number of times so the piece is re-requested
// instead of handing the caller a short piece, which the FUSE layer would report
// as a failed mid-file read. A piece that never arrives still ends as an error.
func (f *raFile) readPiece(ctx context.Context, loader pieceSource, data []byte, start int64) error {
	const (
		attempts = 5
		delay    = 200 * time.Millisecond
	)
	var (
		n   int
		err error
	)
	for attempt := 1; ; attempt++ {
		n, err = loader.ReadAtContext(ctx, data, start, f.readaheadBytes())
		if err == nil && n == len(data) {
			return nil
		}
		if err == io.EOF && n == len(data) {
			// A complete read reported as EOF still carries the whole piece.
			return nil
		}
		short := n < len(data) && (err == nil || errors.Is(err, io.EOF))
		if !short || attempt == attempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if n != len(data) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func (f *raFile) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	window := f.window
	f.window = nil
	coordinator := f.coordinator
	f.mu.Unlock()
	if coordinator != nil {
		coordinator.releaseFile(f)
		return nil
	}
	for _, key := range window {
		f.cache.Unpin(key)
	}
	return nil
}

// windowKeys lists the pieces a read wants kept: the requested range extended
// by readahead.
func (f *raFile) windowKeys(request cache.ReadRequest, length int64) []cache.Key {
	request.Length = length
	plan := cache.Plan(request)
	keys := make([]cache.Key, 0, len(plan.Wanted))
	for _, index := range plan.Wanted {
		keys = append(keys, cache.Key{Torrent: f.torrentKey, Piece: index})
	}
	return keys
}

// protectWindow replaces the pinned read window. The previous window is
// unpinned first, so a seek makes the old region immediately reclaimable while
// the new one is held. A pin that exceeds the cache's pin budget is simply not
// granted: the read then relies on recency instead.
//
// Window replacement and its unpin/pin pair run under one lock. Releasing the
// lock between them lets two concurrent reads interleave as replace(A),
// replace(B), unpin(prev of A), unpin(A), pin(A), pin(B): the stale window A is
// pinned after B became active and stays pinned until some later read or Close
// happens to release it, permanently shrinking what eviction may reclaim.
func (f *raFile) protectWindow(window []cache.Key) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	previous := f.window
	f.window = window
	for _, key := range previous {
		f.cache.Unpin(key)
	}
	if len(window) == 0 {
		return
	}
	if !f.cache.PinSize(window[0], f.pieceSize(window[0].Piece)) && !f.cache.Has(window[0]) {
		// The target is absent and cannot be reserved. Do not pin any future
		// read-ahead key: even a short tail could otherwise consume the last
		// evictable bytes and make the target insertion roll back forever.
		return
	}
	for _, key := range window[1:] {
		f.cache.PinSize(key, f.pieceSize(key.Piece))
	}
}

func (f *raFile) pieceSize(index int) int64 {
	start := int64(index) * f.pieceLength
	if remaining := f.torrentSize - start; remaining < f.pieceLength {
		return remaining
	}
	return f.pieceLength
}
