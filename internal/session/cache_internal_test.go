package session

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/yakumioto/torrentfs-go/internal/cache"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

const internalTestPieceLength = 256 << 10

func internalTestConfig(dataDir string) config.Config {
	cfg := config.Default()
	cfg.Paths.DataDir = dataDir
	return cfg
}

func internalTestTorrentDir(t *testing.T, dataDir string) string {
	t.Helper()
	dir := filepath.Join(filepath.Dir(dataDir), "torrents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	return dir
}

func TestSessionPieceCacheHitCountAndCloseInvalidation(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	content := bytes.Repeat([]byte("cache"), internalTestPieceLength/5+1)
	torrentPath, hash := buildInternalTestTorrent(t, dataDir, work, content)

	sess, err := New(internalTestConfig(dataDir), internalTestTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sess.AddTorrent(ctx, Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	waitInternalTorrentComplete(t, ctx, st)

	ra, err := sess.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	piece := make([]byte, internalTestPieceLength)
	if _, err := ra.ReadAt(piece, 0); err != nil {
		t.Fatalf("first piece read: %v", err)
	}
	key := cache.Key{Torrent: hash.HexString(), Piece: 0}
	if !sess.pieceCache.Has(key) {
		t.Fatal("first piece was not cached")
	}
	before := sess.pieceCache.HitCount()
	if _, err := ra.ReadAt(make([]byte, 32), 1); err != nil {
		t.Fatalf("cached piece read: %v", err)
	}
	if got := sess.pieceCache.HitCount(); got <= before {
		t.Fatalf("HitCount = %d after cached read, want > %d", got, before)
	}

	start := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 32; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			_, _ = ra.ReadAt(make([]byte, 64), 0)
		}()
	}
	close(start)
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	readers.Wait()
	if got := sess.pieceCache.Len(); got != 0 {
		t.Fatalf("cache Len after concurrent close = %d, want 0", got)
	}
}

func TestSessionUsesConfiguredPieceCacheCapacity(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := internalTestConfig(dataDir)
	cfg.Cache.CapacityBytes = 1234

	sess, err := New(cfg, internalTestTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if got := sess.pieceCache.Capacity(); got != 1234 {
		t.Fatalf("piece cache capacity = %d, want 1234", got)
	}
}

func TestRaFileMissCannotPutAfterCloseAndInvalidate(t *testing.T) {
	store := cache.New(64)
	source := &gatedPieceSource{
		started: make(chan struct{}),
		release: make(chan struct{}),
		data:    []byte("piece"),
	}
	file := &raFile{
		loader:      source,
		cache:       store,
		torrentKey:  "old-hash",
		fileSize:    int64(len(source.data)),
		pieceLength: int64(len(source.data)),
		torrentSize: int64(len(source.data)),
	}
	result := make(chan error, 1)
	go func() {
		_, err := file.ReadAt(make([]byte, len(source.data)), 0)
		result <- err
	}()
	<-source.started
	if err := file.Close(); err != nil {
		t.Fatalf("raFile.Close: %v", err)
	}
	store.InvalidateTorrent("old-hash")
	close(source.release)
	if err := <-result; !errors.Is(err, filesystem.ErrClosed) {
		t.Fatalf("ReadAt error = %v, want ErrClosed", err)
	}
	if got := store.Len(); got != 0 {
		t.Fatalf("cache Len after close/invalidate race = %d, want 0", got)
	}
}

type gatedPieceSource struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	data    []byte
}

func (s *gatedPieceSource) ReadAt(dst []byte, off, _ int64) (int, error) {
	s.once.Do(func() { close(s.started) })
	<-s.release
	if off < 0 || off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(dst, s.data[off:])
	return n, nil
}

func (s *gatedPieceSource) ReadAtContext(ctx context.Context, dst []byte, off, readahead int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return s.ReadAt(dst, off, readahead)
}

func (s *gatedPieceSource) Close() error {
	return nil
}

// blockingOperationReader is the torrent.Reader stand-in for loader tests. Its
// reads block until the test releases them or the operation context is
// cancelled, and Seek records the position, so the bytes a read returns prove
// which offset it served.
type blockingOperationReader struct {
	mu sync.Mutex

	ctx         context.Context
	pos         int64
	readCalls   int
	activeReads int
	maxActive   int
	seekCalls   int
	seekOffsets []int64
	readaheads  []int64

	started     chan struct{}
	startedOnce sync.Once
	release     chan struct{}
	releaseOnce sync.Once
}

func newBlockingOperationReader() *blockingOperationReader {
	return &blockingOperationReader{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// releaseReads lets every read blocked in Read, now and later, complete.
func (r *blockingOperationReader) releaseReads() {
	r.releaseOnce.Do(func() { close(r.release) })
}

// blockingPattern is the byte pattern a blocked read serves at position off.
func blockingPattern(off int64, length int) []byte {
	out := make([]byte, length)
	for i := range out {
		out[i] = byte(int64(i) + off)
	}
	return out
}

func (r *blockingOperationReader) SetContext(ctx context.Context) {
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()
}

func (r *blockingOperationReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	ctx := r.ctx
	pos := r.pos
	r.readCalls++
	r.activeReads++
	if r.activeReads > r.maxActive {
		r.maxActive = r.activeReads
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.activeReads--
		r.mu.Unlock()
	}()

	r.startedOnce.Do(func() { close(r.started) })
	select {
	case <-r.release:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	copy(p, blockingPattern(pos, len(p)))
	return len(p), nil
}

func (r *blockingOperationReader) ReadContext(ctx context.Context, p []byte) (int, error) {
	r.SetContext(ctx)
	return r.Read(p)
}

func (r *blockingOperationReader) Seek(off int64, _ int) (int64, error) {
	r.mu.Lock()
	r.seekCalls++
	r.seekOffsets = append(r.seekOffsets, off)
	r.pos = off
	r.mu.Unlock()
	return off, nil
}

func (r *blockingOperationReader) Close() error { return nil }

func (r *blockingOperationReader) SetReadahead(readahead int64) {
	r.mu.Lock()
	r.readaheads = append(r.readaheads, readahead)
	r.mu.Unlock()
}

func (r *blockingOperationReader) SetReadaheadFunc(torrent.ReadaheadFunc) {}

func (r *blockingOperationReader) SetResponsive() {}

func (r *blockingOperationReader) snapshot() (seekCalls, maxActive int, seekOffsets, readaheads []int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seekCalls, r.maxActive, append([]int64(nil), r.seekOffsets...), append([]int64(nil), r.readaheads...)
}

// newProbedLoader builds a loader over a blocking reader with a probe
// installed, and restores the seam and unblocks any leftover read on cleanup.
func newProbedLoader(t *testing.T) (*pieceLoader, *blockingOperationReader, chan readProbeEvent) {
	t.Helper()
	reader := newBlockingOperationReader()
	rootContext, rootCancel := context.WithCancel(context.Background())
	loader := &pieceLoader{r: reader, rootContext: rootContext, rootCancel: rootCancel}
	events := make(chan readProbeEvent, 16)
	restore := SetReadProbe(events)
	t.Cleanup(func() {
		restore()
		reader.releaseReads()
		if err := loader.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return loader, reader, events
}

type loaderReadResult struct {
	n    int
	err  error
	data []byte
}

func startLoaderRead(loader *pieceLoader, ctx context.Context, off int64) <-chan loaderReadResult {
	done := make(chan loaderReadResult, 1)
	go func() {
		buf := make([]byte, 32)
		n, err := loader.ReadAtContext(ctx, buf, off, defaultStreamingReadahead)
		done <- loaderReadResult{n: n, err: err, data: append([]byte(nil), buf...)}
	}()
	return done
}

func waitProbeEvent(t *testing.T, events <-chan readProbeEvent, kind string, offset int64) readProbeEvent {
	t.Helper()
	deadline := time.After(probeWait)
	for {
		select {
		case event := <-events:
			if event.Kind == kind && event.Offset == offset {
				return event
			}
		case <-deadline:
			t.Fatalf("no %q probe event for offset %d", kind, offset)
			return readProbeEvent{}
		}
	}
}

func channelClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func waitChannelClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(probeWait):
		t.Fatalf("%s was never cancelled", what)
	}
}

func waitLoaderResult(t *testing.T, done <-chan loaderReadResult, what string) loaderReadResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(probeWait):
		t.Fatalf("%s did not return", what)
		return loaderReadResult{}
	}
}

func requireNoLoaderResult(t *testing.T, done <-chan loaderReadResult, what string) {
	t.Helper()
	select {
	case res := <-done:
		t.Fatalf("%s returned before the blocking read was released: %d bytes, %v", what, res.n, res.err)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestPieceLoaderOverlappingReadsQueueWithoutCancelling is the regression test
// for the cancellation storm: a second ordinary read that arrives while the
// first is blocked on a missing piece must wait for it, not cancel it. The
// first read's operation context is observed directly, so an implementation
// that cancels the outstanding read fails here even if a fake swallowed the
// cancellation.
func TestPieceLoaderOverlappingReadsQueueWithoutCancelling(t *testing.T) {
	loader, reader, events := newProbedLoader(t)

	firstDone := startLoaderRead(loader, context.Background(), 0)
	first := waitProbeEvent(t, events, "reader-started", 0)
	select {
	case <-reader.started:
	case <-time.After(probeWait):
		t.Fatal("first read never blocked in the underlying reader")
	}

	secondDone := startLoaderRead(loader, context.Background(), 4096)
	waitProbeEvent(t, events, "admission-attempt", 4096)

	if channelClosed(first.Done) {
		t.Fatal("the second read cancelled the first read's operation")
	}
	requireNoLoaderResult(t, firstDone, "first read")
	requireNoLoaderResult(t, secondDone, "second read")

	reader.releaseReads()

	firstResult := waitLoaderResult(t, firstDone, "first read")
	if firstResult.err != nil || firstResult.n != 32 {
		t.Fatalf("first read = %d bytes, %v; want 32, nil", firstResult.n, firstResult.err)
	}
	if want := blockingPattern(0, 32); !bytes.Equal(firstResult.data, want) {
		t.Fatalf("first read data = %v, want %v", firstResult.data, want)
	}
	secondResult := waitLoaderResult(t, secondDone, "second read")
	if secondResult.err != nil || secondResult.n != 32 {
		t.Fatalf("second read = %d bytes, %v; want 32, nil", secondResult.n, secondResult.err)
	}
	if want := blockingPattern(4096, 32); !bytes.Equal(secondResult.data, want) {
		t.Fatalf("second read data = %v, want %v", secondResult.data, want)
	}

	seekCalls, maxActive, seekOffsets, readaheads := reader.snapshot()
	if maxActive != 1 {
		t.Fatalf("maximum concurrent reads = %d, want 1", maxActive)
	}
	if seekCalls != 2 {
		t.Fatalf("Seek calls = %d, want 2", seekCalls)
	}
	if len(seekOffsets) != 2 || seekOffsets[0] != 0 || seekOffsets[1] != 4096 {
		t.Fatalf("Seek offsets = %v, want [0 4096] in order", seekOffsets)
	}
	if len(readaheads) != 2 || readaheads[0] != defaultStreamingReadahead || readaheads[1] != defaultStreamingReadahead {
		t.Fatalf("readaheads = %v, want two [%d] values", readaheads, defaultStreamingReadahead)
	}
}

// TestPieceLoaderCloseCancelsBlockedRead checks the lifecycle half of the
// contract: only closing the loader (not another read) cancels a read blocked
// on unavailable data, and Close still returns instead of deadlocking on the
// admission lock the blocked read holds.
func TestPieceLoaderCloseCancelsBlockedRead(t *testing.T) {
	loader, reader, events := newProbedLoader(t)

	readDone := startLoaderRead(loader, context.Background(), 0)
	started := waitProbeEvent(t, events, "reader-started", 0)
	select {
	case <-reader.started:
	case <-time.After(probeWait):
		t.Fatal("read never blocked in the underlying reader")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- loader.Close() }()

	waitChannelClosed(t, started.Done, "operation context")

	result := waitLoaderResult(t, readDone, "blocked read")
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("blocked read error = %v, want context.Canceled", result.err)
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(probeWait):
		t.Fatal("Close did not return while a read was blocked")
	}
}

// TestPieceLoaderRequestCancellationIsScopedToItsOperation checks that
// cancelling one request's context ends that operation only: a later read with
// its own context starts uncancelled and completes normally.
func TestPieceLoaderRequestCancellationIsScopedToItsOperation(t *testing.T) {
	loader, reader, events := newProbedLoader(t)

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	cancelledDone := startLoaderRead(loader, requestCtx, 0)
	cancelled := waitProbeEvent(t, events, "reader-started", 0)
	select {
	case <-reader.started:
	case <-time.After(probeWait):
		t.Fatal("first read never blocked in the underlying reader")
	}

	cancelRequest()
	cancelledResult := waitLoaderResult(t, cancelledDone, "cancelled read")
	if !errors.Is(cancelledResult.err, context.Canceled) {
		t.Fatalf("cancelled read error = %v, want context.Canceled", cancelledResult.err)
	}
	waitChannelClosed(t, cancelled.Done, "cancelled operation context")

	freshDone := startLoaderRead(loader, context.Background(), 8192)
	fresh := waitProbeEvent(t, events, "reader-started", 8192)
	if channelClosed(fresh.Done) {
		t.Fatal("a fresh read inherited the previous request's cancellation")
	}
	requireNoLoaderResult(t, freshDone, "fresh read")

	reader.releaseReads()
	freshResult := waitLoaderResult(t, freshDone, "fresh read")
	if freshResult.err != nil || freshResult.n != 32 {
		t.Fatalf("fresh read = %d bytes, %v; want 32, nil", freshResult.n, freshResult.err)
	}
	if want := blockingPattern(8192, 32); !bytes.Equal(freshResult.data, want) {
		t.Fatalf("fresh read data = %v, want %v", freshResult.data, want)
	}
}

type recordingPieceSource struct {
	mu         sync.Mutex
	data       []byte
	readaheads []int64
}

func (s *recordingPieceSource) ReadAt(dst []byte, off, readahead int64) (int, error) {
	s.mu.Lock()
	s.readaheads = append(s.readaheads, readahead)
	s.mu.Unlock()
	if off < 0 || off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(dst, s.data[off:])
	if n < len(dst) {
		return n, io.EOF
	}
	return n, nil
}

func (s *recordingPieceSource) Close() error {
	return nil
}

func (s *recordingPieceSource) ReadAtContext(ctx context.Context, dst []byte, off, readahead int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return s.ReadAt(dst, off, readahead)
}

func (s *recordingPieceSource) readaheadValues() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.readaheads...)
}

// TestRaFileOverlappingCacheMissesDoNotCancel is the raFile-level counterpart
// of the loader regression: two files sharing one loader and piece cache, both
// missing, must both return their own bytes instead of the newer read
// cancelling the older one.
func TestRaFileOverlappingCacheMissesDoNotCancel(t *testing.T) {
	loader, reader, events := newProbedLoader(t)
	store := cache.New(128)
	first := &raFile{
		loader:      loader,
		cache:       store,
		torrentKey:  "overlap",
		fileSize:    64,
		pieceLength: 32,
		torrentSize: 64,
	}
	second := &raFile{
		loader:      loader,
		cache:       store,
		torrentKey:  "overlap",
		fileSize:    64,
		pieceLength: 32,
		torrentSize: 64,
	}

	firstDone := make(chan raFileReadResult, 1)
	go func() {
		buf := make([]byte, 32)
		n, err := first.ReadAt(buf, 0)
		firstDone <- raFileReadResult{n: n, err: err, data: append([]byte(nil), buf...)}
	}()
	started := waitProbeEvent(t, events, "reader-started", 0)
	select {
	case <-reader.started:
	case <-time.After(probeWait):
		t.Fatal("first cache-miss read never blocked in the underlying reader")
	}

	// The second file's cache miss must queue behind the first rather than
	// cancel it. Its whole piece is already being filled, so once the first
	// read completes the second is served from the cache.
	secondDone := make(chan raFileReadResult, 1)
	go func() {
		buf := make([]byte, 32)
		n, err := second.ReadAt(buf, 32)
		secondDone <- raFileReadResult{n: n, err: err, data: append([]byte(nil), buf...)}
	}()
	waitProbeEvent(t, events, "admission-attempt", 32)

	if channelClosed(started.Done) {
		t.Fatal("the second cache miss cancelled the first read's operation")
	}
	select {
	case res := <-firstDone:
		t.Fatalf("first cache miss returned before release: %v", res.err)
	case <-time.After(100 * time.Millisecond):
	}

	reader.releaseReads()

	firstResult := waitRaFileResult(t, firstDone, "first cache miss")
	if firstResult.err != nil {
		t.Fatalf("first cache miss: %v", firstResult.err)
	}
	if want := blockingPattern(0, 32); firstResult.n != len(want) || !bytes.Equal(firstResult.data, want) {
		t.Fatalf("first cache miss data = %v, want %v", firstResult.data, want)
	}
	secondResult := waitRaFileResult(t, secondDone, "second cache miss")
	if secondResult.err != nil {
		t.Fatalf("second cache miss: %v", secondResult.err)
	}
	// The offsets must be served by their own positions: a loader that skipped
	// the Seek would hand back the previous position's bytes.
	if want := blockingPattern(32, 32); secondResult.n != len(want) || !bytes.Equal(secondResult.data, want) {
		t.Fatalf("second cache miss data = %v, want %v", secondResult.data, want)
	}
}

// raFileReadResult is one raFile read outcome captured with its bytes.
type raFileReadResult struct {
	n    int
	err  error
	data []byte
}

func waitRaFileResult(t *testing.T, done <-chan raFileReadResult, what string) raFileReadResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(probeWait):
		t.Fatalf("%s did not return", what)
		return raFileReadResult{}
	}
}

// contextPieceSource blocks until its context is done and reports the
// cancellation, proving a request context reached the loader.
type contextPieceSource struct {
	started chan struct{}
	once    sync.Once
}

func (s *contextPieceSource) ReadAtContext(ctx context.Context, _ []byte, _, _ int64) (int, error) {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return 0, ctx.Err()
}

func (s *contextPieceSource) Close() error { return nil }

// TestRaFileReadAtContextReachesLoaderThroughCacheMiss checks that the
// context-aware read path propagates the request context through the
// span/piece cache-miss path into the loader, so a cancelled request ends its
// own read.
func TestRaFileReadAtContextReachesLoaderThroughCacheMiss(t *testing.T) {
	source := &contextPieceSource{started: make(chan struct{})}
	store := cache.New(64)
	file := &raFile{
		loader:      source,
		cache:       store,
		torrentKey:  "ctx-propagation",
		fileSize:    64,
		pieceLength: 64,
		torrentSize: 64,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := file.ReadAtContext(ctx, make([]byte, 8), 0)
		done <- err
	}()

	select {
	case <-source.started:
	case <-time.After(probeWait):
		t.Fatal("cache-miss read never reached the loader")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled cache-miss read error = %v, want context.Canceled", err)
		}
	case <-time.After(probeWait):
		t.Fatal("cancelled cache-miss read did not return")
	}
	if got := store.Len(); got != 0 {
		t.Fatalf("cache Len after a cancelled read = %d, want 0", got)
	}
}

func TestRaFileUsesFixedStreamingReadahead(t *testing.T) {
	source := &recordingPieceSource{data: []byte("piece")}
	file := &raFile{
		loader:      source,
		cache:       cache.New(64),
		torrentKey:  "streaming",
		fileSize:    int64(len(source.data)),
		pieceLength: int64(len(source.data)),
		torrentSize: int64(len(source.data)),
	}
	got := make([]byte, len(source.data))
	if n, err := file.ReadAt(got, 0); err != nil || n != len(got) {
		t.Fatalf("ReadAt = %d bytes, %v; want %d, nil", n, err, len(got))
	}
	if string(got) != string(source.data) {
		t.Fatalf("ReadAt data = %q, want %q", got, source.data)
	}
	if got := source.readaheadValues(); len(got) != 1 || got[0] != defaultStreamingReadahead {
		t.Fatalf("readaheads = %v, want [%d]", got, defaultStreamingReadahead)
	}
}

type unstablePieceStateSource struct {
	calls int
}

func (s *unstablePieceStateSource) PieceStateRuns() torrent.PieceStateRuns {
	return torrent.PieceStateRuns{{
		PieceState: torrent.PieceState{
			Completion: storage.Completion{Ok: true},
			Partial:    true,
		},
		Length: 1,
	}}
}

func (s *unstablePieceStateSource) PieceBytesMissing(int) int64 {
	s.calls++
	return int64(s.calls * 10)
}

func (s *unstablePieceStateSource) Length() int64 {
	return 100
}

func TestPieceSnapshotRejectsUnstableSource(t *testing.T) {
	source := &unstablePieceStateSource{}
	states, err := pieceStatesSnapshot(source, &metainfo.Info{PieceLength: 100})
	if !errors.Is(err, errPieceStateSnapshotUnstable) {
		t.Fatalf("pieceStatesSnapshot error = %v, want unstable snapshot error", err)
	}
	if states != nil {
		t.Fatalf("pieceStatesSnapshot states = %+v, want nil", states)
	}
}

func TestSessionLargePieceReadBypassesCache(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	content := []byte("small request from a large piece")
	torrentPath, hash := buildInternalTestTorrentWithPieceLength(t, dataDir, work, content, config.Default().Cache.CapacityBytes+1)

	sess, err := New(internalTestConfig(dataDir), internalTestTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sess.AddTorrent(ctx, Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	waitInternalTorrentComplete(t, ctx, st)

	ra, err := sess.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	got := make([]byte, len(content))
	if n, err := ra.ReadAt(got, 0); err != nil || n != len(content) {
		t.Fatalf("large-piece read = %d bytes, %v; want %d, nil", n, err, len(content))
	}
	if string(got) != string(content) {
		t.Fatalf("large-piece content = %q, want %q", got, content)
	}
	if got := sess.pieceCache.Len(); got != 0 {
		t.Fatalf("cache Len after oversized piece read = %d, want 0", got)
	}
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func buildInternalTestTorrent(t *testing.T, dataDir, torrentDir string, content []byte) (string, metainfo.Hash) {
	return buildInternalTestTorrentWithPieceLength(t, dataDir, torrentDir, content, internalTestPieceLength)
}

func buildInternalTestTorrentWithPieceLength(t *testing.T, dataDir, torrentDir string, content []byte, pieceLength int64) (string, metainfo.Hash) {
	t.Helper()
	pieceSize := int(pieceLength)
	pieces := make([]byte, 0, (len(content)+pieceSize-1)/pieceSize*sha1.Size)
	for off := 0; off < len(content); off += pieceSize {
		end := off + pieceSize
		if end > len(content) {
			end = len(content)
		}
		sum := sha1.Sum(content[off:end])
		pieces = append(pieces, sum[:]...)
	}
	info := metainfo.Info{
		Name:        "payload.bin",
		Length:      int64(len(content)),
		PieceLength: pieceLength,
		Pieces:      pieces,
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatalf("encode info: %v", err)
	}
	mi := metainfo.MetaInfo{InfoBytes: bencode.Bytes(infoBytes)}
	encoded, err := bencode.Marshal(mi)
	if err != nil {
		t.Fatalf("encode metainfo: %v", err)
	}
	hash := mi.HashInfoBytes()
	dir := filepath.Join(dataDir, "payload", hash.HexString())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make data dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "payload.bin"), content, 0o644); err != nil {
		t.Fatalf("write data: %v", err)
	}
	path := filepath.Join(torrentDir, "payload.bin.torrent")
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatalf("write torrent: %v", err)
	}
	return path, hash
}

func waitInternalTorrentComplete(t *testing.T, ctx context.Context, st *Torrent) {
	t.Helper()
	select {
	case <-st.GotInfo():
	case <-ctx.Done():
		t.Fatalf("timed out waiting for torrent info: %v", ctx.Err())
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if st.BytesCompleted() == st.Length() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("torrent never became complete: %d/%d", st.BytesCompleted(), st.Length())
}
