package api

import (
	"context"
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
	// for the same torrent.
	StartPieceDownload(ctx context.Context, id string, ranges []session.PieceRange) (session.PieceDownloadStatus, error)
	// PieceDownloadStatus returns the current or most recent piece download
	// of one torrent.
	PieceDownloadStatus(id string) (session.PieceDownloadStatus, error)
}

// pieceRangeBody is the wire form of a half-open piece index range.
type pieceRangeBody struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// pieceDownloadRequestBody is the PUT /api/v1/torrents/{id}/piece-download
// request contract.
type pieceDownloadRequestBody struct {
	Ranges []pieceRangeBody `json:"ranges"`
}

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

func (s *Server) handleStartPieceDownload(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.backend.(PieceDownloadBackend)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "piece download unavailable")
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
	ranges := make([]session.PieceRange, 0, len(body.Ranges))
	for _, raw := range body.Ranges {
		ranges = append(ranges, session.PieceRange{Start: raw.Start, End: raw.End})
	}
	status, err := backend.StartPieceDownload(r.Context(), r.PathValue("id"), ranges)
	if err != nil {
		writeSessionError(w, "start piece download", err)
		return
	}
	writeJSON(w, http.StatusAccepted, newPieceDownloadResponse(status))
}

func (s *Server) handleGetPieceDownload(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.backend.(PieceDownloadBackend)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "piece download unavailable")
		return
	}
	status, err := backend.PieceDownloadStatus(r.PathValue("id"))
	if err != nil {
		writeSessionError(w, "get piece download", err)
		return
	}
	writeJSON(w, http.StatusOK, newPieceDownloadResponse(status))
}
