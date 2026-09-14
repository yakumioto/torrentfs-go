package filesystem

import (
	"bytes"
	"context"
	"syscall"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/yakumioto/torrentfs-go/internal/status"
)

// statsFileNode is a read-only status leaf in the stats/ control tree. It
// mirrors one data file of one torrent and renders that file's piece-state
// slice, taken fresh on every open; nothing is written to disk and no media
// bytes are involved.
type statsFileNode struct {
	fs.Inode
	state *fsState
	hash  metainfo.Hash
	path  string // torrent-relative display path of the mirrored data file
}

func (n *statsFileNode) snapshot() ([]byte, error) {
	states, err := n.state.backend.FilePieceStates(n.hash, n.path)
	if err != nil {
		return nil, err
	}
	return []byte(status.Render(states)), nil
}

func (n *statsFileNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	content, err := n.snapshot()
	if err != nil {
		return errnoFor(err)
	}
	out.Mode = 0o444
	out.Size = uint64(len(content))
	return 0
}

func (n *statsFileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY || flags&(syscall.O_TRUNC|syscall.O_CREAT|syscall.O_APPEND) != 0 {
		return nil, 0, errnoFor(ErrReadOnly)
	}
	content, err := n.snapshot()
	if err != nil {
		return nil, 0, errnoFor(err)
	}
	return &readHandle{ra: bytes.NewReader(content), size: int64(len(content))}, 0, 0
}

var (
	_ fs.NodeGetattrer = (*statsFileNode)(nil)
	_ fs.NodeOpener    = (*statsFileNode)(nil)
)
