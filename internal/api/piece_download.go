package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

// PieceDownloadBackend is the optional piece-download management surface.
// Keeping it separate from Backend lets existing payload-only API fakes
// continue to serve the other routes.
type PieceDownloadBackend interface {
	// StartPieceDownload fetches ranges into the torrent's piece cache
	// without opening a file reader, replacing any previous piece download
	// for the same torrent. timeout bounds how long it may run before it is
	// cut off and moved to the expired state.
	StartPieceDownload(ctx context.Context, id string, ranges []session.PieceRange, timeout time.Duration) (session.PieceDownloadStatus, error)
	// PieceDownloadStatus returns the current or most recent piece download
	// of one torrent.
	PieceDownloadStatus(id string) (session.PieceDownloadStatus, error)
	// CancelPieceDownload stops a torrent's in-flight piece download. It is
	// idempotent: no piece download, or one already in a terminal state,
	// is not an error.
	CancelPieceDownload(id string) error
}

// pieceRangeBody is the wire form of a half-open piece index range.
type pieceRangeBody struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// pieceDownloadRequestBody is the PUT /api/v1/torrents/{id}/piece-download
// request contract. TimeoutSeconds is a pointer so an omitted field (use the
// default) is distinguishable from an explicit, invalid 0.
type pieceDownloadRequestBody struct {
	Ranges         []pieceRangeBody `json:"ranges"`
	TimeoutSeconds *int             `json:"timeout_seconds"`
}

const (
	defaultPieceDownloadTimeoutSeconds = 600
	minPieceDownloadTimeoutSeconds     = 1
	maxPieceDownloadTimeoutSeconds     = 3600
)

// pieceDownloadResponse is the wire form of a piece download's status.
// FinishedAt is omitted while the download is running, so a client can branch
// on its presence instead of comparing against the zero time.
type pieceDownloadResponse struct {
	State           string     `json:"state"`
	RequestedPieces int        `json:"requested_pieces"`
	DonePieces      int        `json:"done_pieces"`
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at"`
	Error           string     `json:"error"`
}

func newPieceDownloadResponse(status session.PieceDownloadStatus) pieceDownloadResponse {
	out := pieceDownloadResponse{
		State:           string(status.State),
		RequestedPieces: status.RequestedPieces,
		DonePieces:      status.DonePieces,
		StartedAt:       status.StartedAt,
		Error:           status.Error,
	}
	if !status.FinishedAt.IsZero() {
		finishedAt := status.FinishedAt
		out.FinishedAt = &finishedAt
	}
	return out
}

// pieceDownloadBackend returns the session behind this server as a piece
// download backend, or answers 503 when the backend does not implement it.
func (s *Server) pieceDownloadBackend(w http.ResponseWriter) (PieceDownloadBackend, bool) {
	backend, ok := s.backend.(PieceDownloadBackend)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "piece download unavailable")
		return nil, false
	}
	return backend, true
}

func (s *Server) handleStartPieceDownload(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.pieceDownloadBackend(w)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUploadBytes())
	var body pieceDownloadRequestBody
	if err := decodeJSON(r.Body, &body); err != nil {
		if isTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(body.Ranges) == 0 {
		writeError(w, http.StatusBadRequest, "ranges is required and must not be empty")
		return
	}
	timeoutSeconds := defaultPieceDownloadTimeoutSeconds
	if body.TimeoutSeconds != nil {
		timeoutSeconds = *body.TimeoutSeconds
	}
	if timeoutSeconds < minPieceDownloadTimeoutSeconds || timeoutSeconds > maxPieceDownloadTimeoutSeconds {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"timeout_seconds must be between %d and %d", minPieceDownloadTimeoutSeconds, maxPieceDownloadTimeoutSeconds))
		return
	}
	ranges := make([]session.PieceRange, 0, len(body.Ranges))
	for _, raw := range body.Ranges {
		ranges = append(ranges, session.PieceRange{Start: raw.Start, End: raw.End})
	}
	timeout := time.Duration(timeoutSeconds) * time.Second
	status, err := backend.StartPieceDownload(r.Context(), r.PathValue("id"), ranges, timeout)
	if err != nil {
		writeSessionError(w, "start piece download", err)
		return
	}
	writeJSON(w, http.StatusAccepted, newPieceDownloadResponse(status))
}

func (s *Server) handleGetPieceDownload(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.pieceDownloadBackend(w)
	if !ok {
		return
	}
	status, err := backend.PieceDownloadStatus(r.PathValue("id"))
	if err != nil {
		writeSessionError(w, "get piece download", err)
		return
	}
	writeJSON(w, http.StatusOK, newPieceDownloadResponse(status))
}

func (s *Server) handleCancelPieceDownload(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.pieceDownloadBackend(w)
	if !ok {
		return
	}
	if err := backend.CancelPieceDownload(r.PathValue("id")); err != nil {
		writeSessionError(w, "cancel piece download", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
