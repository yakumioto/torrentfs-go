package session

import (
	"io"
	"sync"

	"github.com/anacrolix/torrent"
)

// raFile adapts a torrent.Reader to io.ReaderAt. A torrent.Reader has a
// single read head and is not safe for concurrent use, so seek+read is
// serialised under a mutex. This matches the M0 spike conclusion: concurrent
// reads inside a session are serialised, which M1 accepts.
type raFile struct {
	mu sync.Mutex
	r  torrent.Reader
}

var _ io.ReaderAt = (*raFile)(nil)
var _ io.Closer = (*raFile)(nil)

// ReadAt implements io.ReaderAt. A torrent.Reader blocks until the requested
// range is available locally, so callers must only read ranges whose pieces
// are complete (M1 has no caching or prioritisation).
func (f *raFile) ReadAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	return f.r.Close()
}
