package session

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/anacrolix/torrent"

	"github.com/yakumioto/torrentfs-go/internal/cache"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

type pieceSource interface {
	prepare(*torrent.Torrent, int, int, int64) error
	ReadAt([]byte, int64) (int, error)
	Close() error
}

// pieceLoader serializes access to one whole-torrent anacrolix reader.
type pieceLoader struct {
	mu     sync.Mutex
	r      torrent.Reader
	cancel context.CancelFunc
	closed bool
	err    error
}

var _ pieceSource = (*pieceLoader)(nil)

func newPieceLoader(t *torrent.Torrent) *pieceLoader {
	ctx, cancel := context.WithCancel(context.Background())
	r := t.NewReader()
	r.SetContext(ctx)
	return &pieceLoader{r: r, cancel: cancel}
}

func (l *pieceLoader) prepare(t *torrent.Torrent, begin, end int, readahead int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return filesystem.ErrClosed
	}
	t.DownloadPieces(begin, end)
	l.r.SetReadahead(readahead)
	return nil
}

func (l *pieceLoader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, filesystem.ErrInvalidName
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, filesystem.ErrClosed
	}
	if _, err := l.r.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(l.r, p)
	if err == io.ErrUnexpectedEOF {
		err = io.EOF
	}
	return n, err
}

func (l *pieceLoader) Close() error {
	l.cancel()
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
	tor         *torrent.Torrent
	torrentKey  string
	fileOffset  int64
	fileSize    int64
	pieceLength int64
	torrentSize int64
	closed      bool
}

var _ io.ReaderAt = (*raFile)(nil)
var _ io.Closer = (*raFile)(nil)

func (f *raFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, filesystem.ErrInvalidName
	}
	if len(p) == 0 {
		return 0, nil
	}

	f.mu.RLock()
	if f.closed {
		f.mu.RUnlock()
		return 0, filesystem.ErrClosed
	}
	request := cache.ReadRequest{
		FileOffset:    off,
		Length:        int64(len(p)),
		FileStart:     f.fileOffset,
		FileSize:      f.fileSize,
		PieceLength:   f.pieceLength,
		TorrentLength: f.torrentSize,
	}
	f.mu.RUnlock()

	plan := cache.Plan(request)
	if len(plan.Spans) == 0 {
		return 0, io.EOF
	}

	needsLoad := false
	for _, span := range plan.Spans {
		if !f.cache.Has(cache.Key{Torrent: f.torrentKey, Piece: span.Index}) {
			needsLoad = true
			break
		}
	}
	if needsLoad {
		if err := f.requestPieces(plan); err != nil {
			return 0, err
		}
	}

	written := 0
	for _, span := range plan.Spans {
		data, err := f.span(span)
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

func (f *raFile) requestPieces(plan cache.ReadPlan) error {
	f.mu.RLock()
	if f.closed {
		f.mu.RUnlock()
		return filesystem.ErrClosed
	}
	loader, tor := f.loader, f.tor
	f.mu.RUnlock()
	return loader.prepare(tor, plan.Wanted[0], plan.Wanted[len(plan.Wanted)-1]+1, plan.Readahead)
}

func (f *raFile) span(pieceSpan cache.PieceSpan) ([]byte, error) {
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
		n, err := f.loader.ReadAt(data, start)
		if err != nil && !(err == io.EOF && n == len(data)) {
			return nil, err
		}
		if n != len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		return data, nil
	}

	data, err := f.piece(pieceSpan.Index)
	if err != nil {
		return nil, err
	}
	if end > int64(len(data)) {
		return nil, io.ErrUnexpectedEOF
	}
	return data[pieceSpan.Offset:end], nil
}

func (f *raFile) piece(index int) ([]byte, error) {
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
	n, err := f.loader.ReadAt(data, start)
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
