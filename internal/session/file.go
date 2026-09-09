package session

import (
	"errors"
	"io"
	"sync"

	"github.com/anacrolix/torrent"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

// raFile adapts a torrent.Reader to io.ReaderAt. A torrent.Reader has a
// single read head and is not safe for concurrent use, so seek+read is
// serialised under a mutex. Cancellation happens before that mutex is taken
// when the session closes, which wakes a reader blocked on unavailable data.
type raFile struct {
	mu     sync.Mutex
	r      torrent.Reader
	cancel func()
	closed bool
	err    error
}

var _ io.ReaderAt = (*raFile)(nil)
var _ io.Closer = (*raFile)(nil)

func (f *raFile) cancelRead() {
	if f.cancel != nil {
		f.cancel()
	}
}

// ReadAt implements io.ReaderAt. A torrent.Reader blocks until the requested
// range is available locally, so callers must only read ranges whose pieces
// are complete (M1 has no caching or prioritisation).
func (f *raFile) ReadAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, filesystem.ErrClosed
	}
	if _, err := f.r.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(f.r, p)
	if err == io.ErrUnexpectedEOF {
		// Reached EOF before the buffer filled: io.ReaderAt reports a short
		// read as io.EOF, not io.ErrUnexpectedEOF.
		err = io.EOF
	}
	return n, err
}

func (f *raFile) Close() error {
	f.cancelRead()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return f.err
	}
	f.closed = true
	f.err = f.r.Close()
	if f.err != nil && errors.Is(f.err, filesystem.ErrClosed) {
		return nil
	}
	return f.err
}
