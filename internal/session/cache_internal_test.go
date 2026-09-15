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

func (s *gatedPieceSource) Close() error {
	return nil
}

type operationReader struct {
	mu sync.Mutex

	ctx context.Context

	started     chan struct{}
	startedOnce sync.Once
	readCalls   int
	seekCalls   int
	activeReads int
	maxActive   int
	readaheads  []int64
}

func (r *operationReader) SetContext(ctx context.Context) {
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()
}

func (r *operationReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	ctx := r.ctx
	r.readCalls++
	call := r.readCalls
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

	if call == 1 {
		r.startedOnce.Do(func() { close(r.started) })
		<-ctx.Done()
		return 0, ctx.Err()
	}
	for i := range p {
		p[i] = byte(i)
	}
	return len(p), nil
}

func (r *operationReader) ReadContext(ctx context.Context, p []byte) (int, error) {
	r.SetContext(ctx)
	return r.Read(p)
}

func (r *operationReader) Seek(off int64, _ int) (int64, error) {
	r.mu.Lock()
	r.seekCalls++
	r.mu.Unlock()
	return off, nil
}

func (r *operationReader) Close() error {
	return nil
}

func (r *operationReader) SetReadahead(readahead int64) {
	r.mu.Lock()
	r.readaheads = append(r.readaheads, readahead)
	r.mu.Unlock()
}

func (r *operationReader) SetReadaheadFunc(torrent.ReadaheadFunc) {}

func (r *operationReader) SetResponsive() {}

func (r *operationReader) snapshot() (seekCalls, maxActive int, readaheads []int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seekCalls, r.maxActive, append([]int64(nil), r.readaheads...)
}

func TestPieceLoaderAdmissionCancellationBeforeReaderLock(t *testing.T) {
	reader := &operationReader{started: make(chan struct{})}
	rootContext, rootCancel := context.WithCancel(context.Background())
	loader := &pieceLoader{
		r:           reader,
		rootContext: rootContext,
		rootCancel:  rootCancel,
	}

	loader.mu.Lock()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { loader.mu.Unlock() }) }
	defer release()

	firstDone := make(chan error, 1)
	go func() {
		_, err := loader.ReadAt(make([]byte, 32), 0, defaultStreamingReadahead)
		firstDone <- err
	}()

	deadline := time.After(time.Second)
	for {
		loader.operationMu.Lock()
		admitted := loader.activeCancel != nil
		loader.operationMu.Unlock()
		if admitted {
			break
		}
		select {
		case <-deadline:
			t.Fatal("ReadAt did not register before waiting for the Reader lock")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	secondDone := make(chan struct {
		n   int
		err error
	}, 1)
	go func() {
		var data [32]byte
		n, err := loader.ReadAt(data[:], 64, defaultStreamingReadahead)
		secondDone <- struct {
			n   int
			err error
		}{n: n, err: err}
	}()
	release()

	select {
	case err := <-firstDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("read waiting for Reader lock error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement read did not cancel the admitted read")
	}
	select {
	case result := <-secondDone:
		if result.err != nil || result.n != 32 {
			t.Fatalf("replacement read = %d bytes, %v; want 32, nil", result.n, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement read did not acquire the loader")
	}
	if err := loader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestPieceLoaderReadCancelsBlockedRead(t *testing.T) {
	reader := &operationReader{started: make(chan struct{})}
	rootContext, rootCancel := context.WithCancel(context.Background())
	loader := &pieceLoader{
		r:           reader,
		rootContext: rootContext,
		rootCancel:  rootCancel,
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := loader.ReadAt(make([]byte, 32), 0, defaultStreamingReadahead)
		firstDone <- err
	}()
	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("initial read did not block")
	}

	secondDone := make(chan struct {
		n   int
		err error
	}, 1)
	go func() {
		var data [32]byte
		n, err := loader.ReadAt(data[:], 128, defaultStreamingReadahead)
		secondDone <- struct {
			n   int
			err error
		}{n: n, err: err}
	}()

	select {
	case err := <-firstDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled read error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement read did not cancel the blocked read")
	}
	select {
	case result := <-secondDone:
		if result.err != nil || result.n != 32 {
			t.Fatalf("replacement read = %d bytes, %v; want 32, nil", result.n, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement read did not complete")
	}

	seekCalls, maxActive, readaheads := reader.snapshot()
	if seekCalls != 2 {
		t.Fatalf("Seek calls = %d, want 2", seekCalls)
	}
	if maxActive != 1 {
		t.Fatalf("maximum concurrent reads = %d, want 1", maxActive)
	}
	if len(readaheads) != 2 || readaheads[0] != defaultStreamingReadahead || readaheads[1] != defaultStreamingReadahead {
		t.Fatalf("readaheads = %v, want two [%d] values", readaheads, defaultStreamingReadahead)
	}
	if err := loader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
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

func (s *recordingPieceSource) readaheadValues() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.readaheads...)
}

func TestRaFileCacheMissPreemptsBlockedRead(t *testing.T) {
	reader := &operationReader{started: make(chan struct{})}
	rootContext, rootCancel := context.WithCancel(context.Background())
	loader := &pieceLoader{
		r:           reader,
		rootContext: rootContext,
		rootCancel:  rootCancel,
	}
	store := cache.New(128)
	first := &raFile{
		loader:      loader,
		cache:       store,
		torrentKey:  "seek-preemption",
		fileSize:    64,
		pieceLength: 32,
		torrentSize: 64,
	}
	second := &raFile{
		loader:      loader,
		cache:       store,
		torrentKey:  "seek-preemption",
		fileSize:    64,
		pieceLength: 32,
		torrentSize: 64,
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := first.ReadAt(make([]byte, 32), 0)
		firstDone <- err
	}()
	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("initial cache-miss read did not block")
	}

	secondDone := make(chan error, 1)
	go func() {
		_, err := second.ReadAt(make([]byte, 32), 32)
		secondDone <- err
	}()
	select {
	case err := <-firstDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("old cache-miss read error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("new cache-miss did not cancel the old read")
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("replacement cache-miss read: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement cache-miss did not complete")
	}
	if err := loader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
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
