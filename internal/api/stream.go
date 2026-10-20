package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"sync"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/media"
	"github.com/yakumioto/torrentfs-go/internal/stream"
)

// StreamBackend is optional: management-only backends need not expose content.
// Each returned reader owns one handle and supports request cancellation.
type StreamBackend interface {
	OpenStream(stream.Location) (stream.File, error)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.backend.(StreamBackend)
	if !s.streamEnabled || !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	location, err := stream.ParsePath(r.URL.EscapedPath())
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if s.streamCtx.Err() != nil {
		writeError(w, http.StatusServiceUnavailable, "server is shutting down")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.streamCtx, cancel)
	defer func() {
		stop()
		cancel()
	}()
	file, err := backend.OpenStream(location)
	if err != nil {
		switch {
		case errors.Is(err, stream.ErrInvalidSignature):
			// The complete path contains a capability. Never put it, its
			// signature, or an underlying error's URL into application logs.
			s.logger.Warn("http stream signature rejected", "remote", r.RemoteAddr, "hash", location.Hash.HexString())
			writeError(w, http.StatusForbidden, "invalid stream signature")
		case errors.Is(err, stream.ErrDisabled), errors.Is(err, filesystem.ErrNotFound):
			http.NotFound(w, r)
		case errors.Is(err, filesystem.ErrClosed):
			writeError(w, http.StatusServiceUnavailable, "session is closed")
		default:
			writeError(w, http.StatusInternalServerError, "stream unavailable")
		}
		return
	}
	if file.Reader == nil {
		writeError(w, http.StatusInternalServerError, "stream unavailable")
		return
	}
	defer func() {
		// ServeContent can have a multipart producer still unwinding when a
		// response write fails. Cancel its reads before releasing this handle.
		cancel()
		_ = file.Reader.Close()
	}()
	if file.Size < 0 {
		writeError(w, http.StatusInternalServerError, "stream unavailable")
		return
	}

	w.Header().Set("Content-Type", media.ContentType(location.Path))
	w.Header().Set("ETag", `"`+location.Hash.HexString()+"-"+location.Signature+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	reader := &streamReaderAt{ctx: ctx, reader: file.Reader}
	content := io.NewSectionReader(reader, 0, file.Size)
	http.ServeContent(w, r, path.Base(location.Path), file.ModifiedAt, content)
	if err := reader.failure(); err != nil {
		// ServeContent deliberately ignores io.CopyN errors. Returning normally
		// would make an HTTP/2 response look complete even when content stopped
		// early. Abort the transport rather than append an error to media bytes.
		if ctx.Err() == nil {
			reason := "read failed"
			if errors.Is(err, context.DeadlineExceeded) {
				reason = "read timed out"
			} else if errors.Is(err, io.ErrUnexpectedEOF) {
				reason = "short read"
			}
			s.logger.Warn("http stream interrupted", "hash", location.Hash.HexString(), "reason", reason)
		}
		panic(http.ErrAbortHandler)
	}
}

// streamReaderAt keeps cancellation on the request rather than openedFile's
// Background-based ReaderAt fallback. It also records premature EOF, which
// must not be mistaken for successful completion of a known-length file.
type streamReaderAt struct {
	ctx    context.Context
	reader stream.Reader
	mu     sync.Mutex
	err    error
}

func (r *streamReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		r.record(err)
		return 0, err
	}
	n, err := r.reader.ReadAtContext(r.ctx, p, off)
	if n < len(p) && (err == nil || errors.Is(err, io.EOF)) {
		err = io.ErrUnexpectedEOF
	}
	if err != nil && !(errors.Is(err, io.EOF) && n == len(p)) {
		r.record(err)
	}
	return n, err
}

func (r *streamReaderAt) record(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err == nil {
		r.err = err
	}
}

func (r *streamReaderAt) failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}
