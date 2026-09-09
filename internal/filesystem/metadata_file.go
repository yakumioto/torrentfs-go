package filesystem

import (
	"context"
	"io"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type metadataFileNode struct {
	fs.Inode
	state *fsState
	name  string

	mu   sync.Mutex
	size int64
}

func (n *metadataFileNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	n.mu.Lock()
	out.Size = uint64(n.size)
	n.mu.Unlock()
	out.Mode = 0o644
	return 0
}

func (n *metadataFileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY || flags&(syscall.O_TRUNC|syscall.O_CREAT) != 0 {
		return nil, 0, errnoFor(ErrExists)
	}
	name, _ := n.Parent()
	n.mu.Lock()
	if name == "" {
		name = n.name
	}
	size := n.size
	n.mu.Unlock()
	reader, err := n.state.metadata.OpenMetadata(name)
	if err != nil {
		return nil, 0, errnoFor(err)
	}
	return &metadataReadHandle{reader: reader, size: size}, 0, 0
}

type metadataReadHandle struct {
	reader MetadataReader
	size   int64
}

var _ fs.FileReader = (*metadataReadHandle)(nil)
var _ fs.FileReleaser = (*metadataReadHandle)(nil)

func (h *metadataReadHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if off < 0 {
		return nil, errnoFor(ErrInvalidName)
	}
	if off >= h.size {
		return fuse.ReadResultData(nil), 0
	}
	n, err := h.reader.ReadAt(dest, off)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, errnoFor(err)
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (h *metadataReadHandle) Release(ctx context.Context) syscall.Errno {
	return errnoFor(h.reader.Close())
}

type metadataWriteHandle struct {
	writer MetadataWriter
	node   *metadataFileNode

	mu        sync.Mutex
	committed bool
}

var _ fs.FileWriter = (*metadataWriteHandle)(nil)
var _ fs.FileFlusher = (*metadataWriteHandle)(nil)
var _ fs.FileFsyncer = (*metadataWriteHandle)(nil)
var _ fs.FileReleaser = (*metadataWriteHandle)(nil)

func (h *metadataWriteHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	if off < 0 {
		return 0, errnoFor(ErrInvalidName)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.committed {
		return 0, errnoFor(ErrClosed)
	}
	n, err := h.writer.WriteAt(data, off)
	if err != nil {
		return uint32(n), errnoFor(err)
	}
	h.node.mu.Lock()
	if end := off + int64(n); end > h.node.size {
		h.node.size = end
	}
	h.node.mu.Unlock()
	return uint32(n), 0
}

func (h *metadataWriteHandle) commit() syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.committed {
		return 0
	}
	if err := h.writer.Commit(); err != nil {
		return errnoFor(err)
	}
	h.committed = true
	return 0
}

func (h *metadataWriteHandle) Flush(ctx context.Context) syscall.Errno {
	return h.commit()
}

func (h *metadataWriteHandle) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	return h.commit()
}

func (h *metadataWriteHandle) Release(ctx context.Context) syscall.Errno {
	h.mu.Lock()
	committed := h.committed
	h.mu.Unlock()
	if committed {
		return 0
	}
	return errnoFor(h.writer.Abort())
}

var _ fs.NodeGetattrer = (*metadataFileNode)(nil)
var _ fs.NodeOpener = (*metadataFileNode)(nil)
