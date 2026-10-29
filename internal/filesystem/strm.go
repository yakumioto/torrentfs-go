package filesystem

import "github.com/yakumioto/torrentfs-go/internal/vfs"

// Compatibility aliases keep FUSE callers on the shared, protocol-independent
// STRM projection rather than maintaining a second implementation.
type StreamURLBackend = vfs.StreamURLBackend

func NewSTRMBackend(backend StreamURLBackend) Backend {
	return vfs.NewSTRMBackend(backend)
}
