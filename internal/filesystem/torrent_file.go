package filesystem

import (
	"context"
	"io"
	"syscall"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// torrentFileNode is a read-only leaf file inside a torrent. Content is read
// on demand through the Backend; nothing is cached and nothing is written.
type torrentFileNode struct {
	fs.Inode
	state      *fsState
	hash       metainfo.Hash
	path       string // torrent-relative display path
	size       int64
	createdAt  time.Time
	subtitle   bool
	modifiedAt time.Time
}

func (n *torrentFileNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o444
	out.Size = uint64(n.size)
	out.Nlink = 1
	if n.subtitle {
		setModifiedAt(&out.Attr, n.modifiedAt)
	} else {
		setCreatedAt(&out.Attr, n.createdAt)
	}
	return 0
}

func (n *torrentFileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if acc := flags & syscall.O_ACCMODE; acc != syscall.O_RDONLY {
		return nil, 0, errnoFor(ErrReadOnly)
	}
	if flags&syscall.O_TRUNC != 0 {
		return nil, 0, errnoFor(ErrReadOnly)
	}
	var (
		ra  io.ReaderAt
		err error
	)
	if n.subtitle {
		backend, ok := n.state.backend.(SubtitleBackend)
		if !ok {
			return nil, 0, errnoFor(ErrNotFound)
		}
		ra, err = backend.OpenSubtitle(n.hash, n.path)
	} else {
		ra, err = n.state.backend.OpenFile(n.hash, n.path)
	}
	if err != nil {
		return nil, 0, errnoFor(err)
	}
	return &readHandle{ra: ra, size: n.size}, 0, 0
}

// readHandle serves reads for one opened torrent file. Release closes only the
// lightweight handle; the session keeps shared torrent state alive.
type readHandle struct {
	ra   io.ReaderAt
	size int64
}

var _ fs.FileReader = (*readHandle)(nil)
var _ fs.FileReleaser = (*readHandle)(nil)

// contextualReaderAt is the optional backend extension that lets a FUSE read
// pass its request context down to the reader. Backends that only implement
// io.ReaderAt keep working through the fallback below.
type contextualReaderAt interface {
	ReadAtContext(context.Context, []byte, int64) (int, error)
}

func (h *readHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if off < 0 {
		return nil, errnoFor(ErrInvalidName)
	}
	if off >= h.size {
		return fuse.ReadResultData(nil), 0
	}
	var (
		n   int
		err error
	)
	if ra, ok := h.ra.(contextualReaderAt); ok {
		n, err = ra.ReadAtContext(ctx, dest, off)
	} else {
		n, err = h.ra.ReadAt(dest, off)
	}
	if err != nil && err != io.EOF {
		return nil, errnoFor(err)
	}
	if n < len(dest) && off+int64(n) < h.size {
		// A short read that stops before the end of the file is a failed read,
		// not end of file: the data exists, it just was not produced. Reporting
		// it as a successful short read makes the kernel zero-fill the rest of
		// the page and mark it up to date, so every later read of that page
		// returns those zeros. An error leaves the page uncached so a retry can
		// still produce the real bytes.
		return nil, errnoFor(io.ErrUnexpectedEOF)
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (h *readHandle) Release(ctx context.Context) syscall.Errno {
	if closer, ok := h.ra.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			return errnoFor(err)
		}
	}
	return 0
}
