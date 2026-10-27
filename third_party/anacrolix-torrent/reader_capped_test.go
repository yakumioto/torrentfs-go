package torrent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/internal/testutil"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

type cappedReaderStore struct {
	capped      atomic.Bool
	complete    atomic.Bool
	reads       atomic.Int32
	opens       atomic.Int32
	closes      atomic.Int32
	read        func(int32, []byte, int64) (int, error)
	closeErr    error
	missing     chan struct{}
	missingOnce sync.Once
}

func (s *cappedReaderStore) OpenTorrent(context.Context, *metainfo.Info, metainfo.Hash) (storage.TorrentImpl, error) {
	capacity := func() (int64, bool) { return 64, s.capped.Load() }
	return storage.TorrentImpl{
		Capacity:      &capacity,
		Close:         func() error { return nil },
		PieceWithHash: func(p metainfo.Piece, _ g.Option[[]byte]) storage.PieceImpl { return cappedReaderPiece{s: s, piece: p} },
		NewReader:     func() storage.TorrentReader { return &cappedStorageReader{s: s, id: s.opens.Add(1)} },
	}, nil
}

type cappedReaderPiece struct {
	s     *cappedReaderStore
	piece metainfo.Piece
}

func (p cappedReaderPiece) ReadAt(b []byte, off int64) (int, error) {
	return bytes.NewReader([]byte(testutil.GreetingFileContents)).ReadAt(b, p.piece.Offset()+off)
}
func (p cappedReaderPiece) WriteAt([]byte, int64) (int, error) { return 0, io.ErrClosedPipe }
func (p cappedReaderPiece) MarkComplete() error                { p.s.complete.Store(true); return nil }
func (p cappedReaderPiece) MarkNotComplete() error             { p.s.complete.Store(false); return nil }
func (p cappedReaderPiece) Completion() storage.Completion {
	complete := p.s.complete.Load()
	if !complete && p.s.missing != nil {
		p.s.missingOnce.Do(func() { close(p.s.missing) })
	}
	return storage.Completion{Ok: true, Complete: complete}
}

type cappedStorageReader struct {
	s  *cappedReaderStore
	id int32
}

func (r *cappedStorageReader) ReadAt(p []byte, off int64) (int, error) {
	r.s.reads.Add(1)
	if r.s.read != nil {
		return r.s.read(r.id, p, off)
	}
	return bytes.NewReader([]byte(testutil.GreetingFileContents)).ReadAt(p, off)
}
func (r *cappedStorageReader) Close() error { r.s.closes.Add(1); return r.s.closeErr }

func newCappedReader(t *testing.T, s *cappedReaderStore, complete bool) (*Torrent, Reader) {
	t.Helper()
	s.capped.Store(true)
	s.complete.Store(complete)
	cfg := TestingConfig(t)
	cfg.DisableUTP = true
	cfg.Slogger = slog.New(slog.NewTextHandler(io.Discard, nil))
	cl, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	spec := TorrentSpecFromMetaInfo(testutil.GreetingMetaInfo())
	spec.Storage = s
	tor, _, err := cl.AddTorrentSpec(spec)
	if err != nil {
		cl.Close()
		t.Fatal(err)
	}
	r := tor.NewReader()
	r.SetReadahead(0)
	t.Cleanup(func() { s.capped.Store(false); _ = r.Close(); cl.Close() })
	return tor, r
}

type cappedReadResult struct {
	n   int
	err error
}

func startCappedRead(r Reader, ctx context.Context, p []byte) <-chan cappedReadResult {
	done := make(chan cappedReadResult, 1)
	go func() { defer close(done); n, err := r.ReadContext(ctx, p); done <- cappedReadResult{n, err} }()
	return done
}
func awaitCappedRead(t *testing.T, s *cappedReaderStore, done <-chan cappedReadResult, abort context.CancelFunc) cappedReadResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(time.Second):
		// Release the old recursive implementation only after the regression has failed.
		s.capped.Store(false)
		abort()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("failed read could not be cleaned up")
		}
		t.Fatal("capped read did not terminate")
		return cappedReadResult{}
	}
}

type readerWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *readerWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestReaderCappedTermination(t *testing.T) {
	for _, which := range []string{"precanceled_missing", "precanceled_readable", "waiting_cancel", "deadline", "torrent_close", "download_disabled", "network_disabled"} {
		t.Run(which, func(t *testing.T) {
			s := &cappedReaderStore{}
			tor, r := newCappedReader(t, s, which == "precanceled_readable")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var want error
			switch which {
			case "precanceled_missing", "precanceled_readable":
				cancel()
				want = context.Canceled
			case "deadline":
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 10*time.Millisecond)
				defer stop()
				want = context.DeadlineExceeded
			}
			wait := &readerWaitContext{Context: ctx, entered: make(chan struct{})}
			done := startCappedRead(r, wait, make([]byte, 1))
			if want == nil {
				select {
				case <-wait.entered:
				case <-time.After(time.Second):
					t.Fatal("reader did not wait for data")
				}
				switch which {
				case "waiting_cancel":
					cancel()
					want = context.Canceled
				case "torrent_close":
					tor.Drop()
					want = errTorrentClosed
				case "download_disabled":
					tor.DisallowDataDownload()
					want = errReaderDownloadDisabled
				case "network_disabled":
					tor.cl.lock()
					tor.networkingEnabled.Clear()
					tor.cl.unlock()
					want = errReaderNetworkingDisabled
				}
			}
			result := awaitCappedRead(t, s, done, cancel)
			if result.n != 0 || !errors.Is(result.err, want) {
				t.Fatalf("read = %d, %v; want %v", result.n, result.err, want)
			}
			if s.reads.Load() != 0 || s.opens.Load() != 0 {
				t.Fatal("terminal request touched storage")
			}
			if which == "precanceled_readable" {
				p := make([]byte, 1)
				if n, err := r.ReadContext(context.Background(), p); n != 1 || err != nil || p[0] != 'h' {
					t.Fatalf("new request poisoned: %d %v", n, err)
				}
			}
		})
	}
}

func TestReaderCappedPersistentErrorsAreBounded(t *testing.T) {
	permanent := errors.New("permanent storage failure")
	closeErr := errors.New("storage close failure")
	for _, which := range []string{"storage", "close", "unchanged_eof"} {
		t.Run(which, func(t *testing.T) {
			s := &cappedReaderStore{read: func(int32, []byte, int64) (int, error) {
				if which == "storage" {
					return 0, fmt.Errorf("wrapped: %w", permanent)
				}
				return 0, io.EOF
			}}
			if which == "close" {
				s.closeErr = closeErr
			}
			_, r := newCappedReader(t, s, true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := awaitCappedRead(t, s, startCappedRead(r, ctx, make([]byte, 1)), cancel)
			want := permanent
			switch which {
			case "close":
				want = closeErr
			case "unchanged_eof":
				want = io.ErrUnexpectedEOF
			}
			if result.n != 0 || !errors.Is(result.err, want) {
				t.Fatalf("read: %d %v, want %v", result.n, result.err, want)
			}
			if s.reads.Load() > 3 {
				t.Fatalf("unbounded recovery reads: %d", s.reads.Load())
			}
			if s.closes.Load() != 1 {
				t.Fatalf("recovery close calls = %d, want 1", s.closes.Load())
			}
		})
	}
}

func TestReaderCappedResetRecovers(t *testing.T) {
	s := &cappedReaderStore{read: func(id int32, p []byte, off int64) (int, error) {
		if id == 1 {
			return 0, io.EOF
		}
		return bytes.NewReader([]byte(testutil.GreetingFileContents)).ReadAt(p, off)
	}}
	_, r := newCappedReader(t, s, true)
	data, err := io.ReadAll(r)
	if err != nil || sha256.Sum256(data) != sha256.Sum256([]byte(testutil.GreetingFileContents)) {
		t.Fatalf("reset data: %q %v", data, err)
	}
	if s.opens.Load() != 2 || s.reads.Load() != 2 {
		t.Fatalf("reset opens/reads=%d/%d", s.opens.Load(), s.reads.Load())
	}
}

func TestReaderCappedEvictionWaitsAndRecovers(t *testing.T) {
	var resident atomic.Bool
	var staleReader atomic.Int32
	s := &cappedReaderStore{missing: make(chan struct{})}
	s.read = func(id int32, p []byte, off int64) (int, error) {
		if !resident.Load() {
			staleReader.Store(id)
			s.complete.Store(false)
			return 0, io.EOF
		}
		if id == staleReader.Load() {
			return 0, io.EOF
		}
		return bytes.NewReader([]byte(testutil.GreetingFileContents)).ReadAt(p, off)
	}
	tor, r := newCappedReader(t, s, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf := make([]byte, len(testutil.GreetingFileContents))
	done := startCappedRead(r, ctx, buf)
	select {
	case <-s.missing:
	case <-time.After(time.Second):
		t.Fatal("completion was not resynchronized")
	}
	select {
	case result := <-done:
		t.Fatalf("eviction did not wait for source: %+v", result)
	default:
	}
	resident.Store(true)
	s.complete.Store(true)
	tor.Piece(0).UpdateCompletion()
	result := awaitCappedRead(t, s, done, cancel)
	if result.n != len(buf) || !errors.Is(result.err, io.EOF) || sha256.Sum256(buf) != sha256.Sum256([]byte(testutil.GreetingFileContents)) {
		t.Fatalf("recovered read: %d %v %q", result.n, result.err, buf)
	}
}

func TestReaderCappedWaitersAreIsolated(t *testing.T) {
	s := &cappedReaderStore{}
	tor, first := newCappedReader(t, s, false)
	second := tor.NewReader()
	second.SetReadahead(0)
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	firstWait := &readerWaitContext{Context: firstCtx, entered: make(chan struct{})}
	secondWait := &readerWaitContext{Context: secondCtx, entered: make(chan struct{})}
	firstDone := startCappedRead(first, firstWait, make([]byte, 1))
	p := make([]byte, 1)
	secondDone := startCappedRead(second, secondWait, p)
	defer func() {
		s.capped.Store(false)
		cancelFirst()
		cancelSecond()
		for range firstDone {
		}
		for range secondDone {
		}
		_ = second.Close()
	}()
	for _, entered := range []chan struct{}{firstWait.entered, secondWait.entered} {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("waiter did not block")
		}
	}
	cancelFirst()
	if result := awaitCappedRead(t, s, firstDone, cancelFirst); !errors.Is(result.err, context.Canceled) {
		t.Fatalf("first waiter: %v", result.err)
	}
	select {
	case result := <-secondDone:
		t.Fatalf("other waiter canceled: %+v", result)
	default:
	}
	s.complete.Store(true)
	tor.Piece(0).UpdateCompletion()
	if result := awaitCappedRead(t, s, secondDone, cancelSecond); result.n != 1 || result.err != nil || p[0] != 'h' {
		t.Fatalf("other waiter: %+v", result)
	}
}

func TestReaderCappedCancellationAfterStorageReadPreservesBytes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &cappedReaderStore{read: func(_ int32, p []byte, off int64) (int, error) {
		n, err := bytes.NewReader([]byte(testutil.GreetingFileContents)).ReadAt(p, off)
		cancel()
		return n, err
	}}
	_, r := newCappedReader(t, s, true)
	p := make([]byte, len(testutil.GreetingFileContents))
	n, err := r.ReadContext(ctx, p)
	if n != len(p) || !errors.Is(err, context.Canceled) || !bytes.Equal(p, []byte(testutil.GreetingFileContents)) {
		t.Fatalf("canceled storage read: %d %v %q", n, err, p)
	}
}

func TestReaderCappedWrappedClosedErrorRetainsIdentity(t *testing.T) {
	storageErr := errors.New("closed storage failure")
	var tor *Torrent
	s := &cappedReaderStore{read: func(int32, []byte, int64) (int, error) {
		tor.Drop()
		return 0, fmt.Errorf("wrapped storage: %w", storageErr)
	}}
	var r Reader
	tor, r = newCappedReader(t, s, true)
	_, err := r.Read(make([]byte, 1))
	if !errors.Is(err, errTorrentClosed) || !errors.Is(err, storageErr) || s.reads.Load() != 1 {
		t.Fatalf("closed identity: %v, reads=%d", err, s.reads.Load())
	}
}

func TestReaderCappedZeroLengthSeekAndEOF(t *testing.T) {
	s := &cappedReaderStore{}
	_, r := newCappedReader(t, s, true)
	if n, err := r.Read(nil); n != 0 || err != nil || s.reads.Load() != 0 {
		t.Fatalf("zero read: %d %v", n, err)
	}
	if _, err := r.Seek(6, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 1)
	if n, err := r.Read(p); n != 1 || err != nil || p[0] != 0 {
		t.Fatalf("seek read: %d %v %v", n, err, p)
	}
	if _, err := r.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if n, err := r.Read(p); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("end read: %d %v", n, err)
	}
}
