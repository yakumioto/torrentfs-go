// Package stream defines signed payload locations and context-aware readers.
package stream

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

var (
	// ErrInvalidSignature identifies a location whose signature does not verify.
	ErrInvalidSignature = errors.New("stream: invalid signature")
	// ErrDisabled identifies a request made when signed streaming is disabled.
	ErrDisabled = errors.New("stream: disabled")
	// ErrMalformed identifies an invalid stream URL or relative payload path.
	ErrMalformed = errors.New("stream: malformed location")
)

// Location identifies a signed original torrent payload path.
type Location struct {
	Hash      metainfo.Hash
	Signature string
	Path      string
}

// Reader supports bounded random access with request cancellation.
type Reader interface {
	io.ReaderAt
	io.Closer
	ReadAtContext(context.Context, []byte, int64) (int, error)
}

// File contains the payload reader and metadata needed for an HTTP response.
type File struct {
	Reader     Reader
	Size       int64
	ModifiedAt time.Time
}
