package api

import (
	"context"
	"net/http"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

// HandoffBackend is the optional handoff management surface. Keeping it
// separate from Backend lets existing payload-only API fakes continue to serve
// the other routes.
type HandoffBackend interface {
	// HandoffTorrent hands one ready torrent off to the configured external
	// downloader and deletes it locally on success. The returned operation
	// reports the delivery and the deletion.
	HandoffTorrent(ctx context.Context, id string) (*session.Operation, error)
	// HandoffTarget reports whether a target is configured.
	HandoffTarget() session.HandoffTargetView
	// TestHandoffTarget connects to the target and reports its version.
	TestHandoffTarget(ctx context.Context) (session.HandoffTargetView, error)
}

// handoffTargetResponse is the wire form of the configured target.
type handoffTargetResponse struct {
	Configured bool   `json:"configured"`
	Target     string `json:"target"`
	// Version is present only on a successful connection test.
	Version string `json:"version,omitempty"`
}

func newHandoffTargetResponse(view session.HandoffTargetView) handoffTargetResponse {
	return handoffTargetResponse{
		Configured: view.Configured,
		Target:     view.Target,
		Version:    view.Version,
	}
}

// handoffBackend returns the session behind this server as a handoff backend,
// or answers 503 when the backend does not implement it.
func (s *Server) handoffBackend(w http.ResponseWriter) (HandoffBackend, bool) {
	backend, ok := s.backend.(HandoffBackend)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "handoff unavailable")
		return nil, false
	}
	return backend, true
}

func (s *Server) handleHandoffTorrent(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.handoffBackend(w)
	if !ok {
		return
	}
	op, err := backend.HandoffTorrent(r.Context(), r.PathValue("id"))
	if err != nil {
		writeSessionError(w, "handoff", err)
		return
	}
	writeJSON(w, http.StatusAccepted, newOperationResponse(*op))
}

func (s *Server) handleGetHandoffTarget(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.handoffBackend(w)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, newHandoffTargetResponse(backend.HandoffTarget()))
}

func (s *Server) handleTestHandoffTarget(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.handoffBackend(w)
	if !ok {
		return
	}
	view, err := backend.TestHandoffTarget(r.Context())
	if err != nil {
		writeSessionError(w, "test handoff target", err)
		return
	}
	writeJSON(w, http.StatusOK, newHandoffTargetResponse(view))
}
