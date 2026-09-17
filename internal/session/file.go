package session

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/anacrolix/torrent"

	"github.com/yakumioto/torrentfs-go/internal/cache"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

// readProbeEvent reports one step of a loader read to a test-installed probe.
// Kind is "admission-attempt" just before the read waits for the admission
// lock, or "reader-started" once the read is admitted and about to enter the
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

// pieceLoader serializes access to one whole-torrent anacrolix reader.
type pieceLoader struct {
	mu sync.Mutex
	// admissionMu serializes operation admission through the full Reader use.
	admissionMu sync.Mutex

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
		r:           r,
		rootContext: ctx,
		rootCancel:  cancel,
	}
}

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
// and a new read waits for the one in flight instead of cancelling it. Only
// the operation's own context (requestCtx, the loader's root context, or
// Close) can end it.
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
	l.admissionMu.Lock()
	defer l.admissionMu.Unlock()
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
	l.admissionMu.Lock()
	defer l.admissionMu.Unlock()
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
	torrentKey  string
	fileOffset  int64
	fileSize    int64
	pieceLength int64
	torrentSize int64
	readahead   int64
	closed      bool
}

var _ io.ReaderAt = (*raFile)(nil)
var _ io.Closer = (*raFile)(nil)

func (f *raFile) ReadAt(p []byte, off int64) (int, error) {
	return f.ReadAtContext(context.Background(), p, off)
}

// ReadAtContext serves one file-local read on behalf of ctx. Cancelling ctx
// ends this read only: it never cancels another read's operation.
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

	written := 0
	for _, span := range plan.Spans {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		data, err := f.span(ctx, loader, span)
		if err != nil {
			return written, err
		}
		written += copy(p[written:], data)
	}
	if written < len(p) {
		return written, io.EOF
	}
	return written, nil
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

	if f.pieceLength > f.cache.Capacity() {
		start := int64(pieceSpan.Index)*f.pieceLength + pieceSpan.Offset
		if start < 0 || start+pieceSpan.Length > f.torrentSize {
			return nil, io.EOF
		}
		data := make([]byte, int(pieceSpan.Length))
		n, err := loader.ReadAtContext(ctx, data, start, f.readaheadBytes())
		if err != nil && !(err == io.EOF && n == len(data)) {
			return nil, err
		}
		if n != len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		return data, nil
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
	n, err := loader.ReadAtContext(ctx, data, start, f.readaheadBytes())
	if err != nil && !(err == io.EOF && n == len(data)) {
		return nil, err
	}
	if n != len(data) {
		return nil, io.ErrUnexpectedEOF
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

func (f *raFile) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}
