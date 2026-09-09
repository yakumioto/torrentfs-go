package filesystem

import (
	"context"
	"io"
	"syscall"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// torrentFileNode is a read-only leaf file inside a torrent. Content is read
// on demand through the Backend; nothing is cached and nothing is written.
type torrentFileNode struct {
	fs.Inode
	state *fsState
	hash  metainfo.Hash
	path  string // torrent-relative display path
	size  int64
}

func (n *torrentFileNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o444
	out.Size = uint64(n.size)
	return 0
}

func (n *torrentFileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if acc := flags & syscall.O_ACCMODE; acc != syscall.O_RDONLY {
		return nil, 0, syscall.EROFS
	}
	if flags&syscall.O_TRUNC != 0 {
		return nil, 0, syscall.EROFS
	}
	ra, err := n.state.backend.OpenFile(n.hash, n.path)
	if err != nil {
		return nil, 0, syscall.EIO
	}
	return &readHandle{ra: ra, size: n.size}, 0, 0
}

// readHandle serves reads for an opened torrent file. Its lifecycle does not
// own the underlying handle: the session caches and closes those, so Release
// is a no-op.
type readHandle struct {
	ra   io.ReaderAt
	size int64
}

var _ fs.FileReader = (*readHandle)(nil)
var _ fs.FileReleaser = (*readHandle)(nil)

func (h *readHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if off < 0 {
		return nil, syscall.EINVAL
	}
	if off >= h.size {
		return fuse.ReadResultData(nil), 0
	}
	n, err := h.ra.ReadAt(dest, off)
	if err != nil && err != io.EOF {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (h *readHandle) Release(ctx context.Context) syscall.Errno {
	return 0
}
