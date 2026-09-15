package api

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

type torrentResponse struct {
	ID             string    `json:"id"`
	InfoHash       string    `json:"info_hash"`
	Name           string    `json:"name"`
	State          string    `json:"state"`
	TotalBytes     int64     `json:"total_bytes"`
	CompletedBytes int64     `json:"completed_bytes"`
	Progress       float64   `json:"progress"`
	CreatedAt      time.Time `json:"created_at"`
	Error          string    `json:"error,omitempty"`
}

type operationResponse struct {
	OperationID string `json:"operation_id"`
	TorrentID   string `json:"torrent_id"`
	State       string `json:"state"`
	PurgeData   bool   `json:"purge_data"`
	Error       string `json:"error,omitempty"`
}

type pieceStatusResponse struct {
	Index          int    `json:"index"`
	Known          bool   `json:"known"`
	Complete       bool   `json:"complete"`
	Partial        bool   `json:"partial"`
	Wanted         bool   `json:"wanted"`
	Checking       bool   `json:"checking"`
	AvailableBytes *int64 `json:"available_bytes,omitempty"`
}

type fileStatusResponse struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	PieceStart int    `json:"piece_start"`
	PieceEnd   int    `json:"piece_end"`
}

type torrentStatusResponse struct {
	Torrent       torrentResponse       `json:"torrent"`
	MetainfoReady bool                  `json:"metainfo_ready"`
	PieceLength   int64                 `json:"piece_length"`
	Pieces        []pieceStatusResponse `json:"pieces"`
	Files         []fileStatusResponse  `json:"files"`
}

func newTorrentResponse(view session.TorrentView) torrentResponse {
	return torrentResponse{
		ID:             view.ID,
		InfoHash:       view.InfoHash,
		Name:           view.Name,
		State:          string(view.State),
		TotalBytes:     view.TotalBytes,
		CompletedBytes: view.CompletedBytes,
		Progress:       view.Progress,
		CreatedAt:      view.CreatedAt,
		Error:          view.Error,
	}
}

func newTorrentStatusResponse(view session.TorrentStatusView) torrentStatusResponse {
	out := torrentStatusResponse{
		Torrent:       newTorrentResponse(view.Torrent),
		MetainfoReady: view.MetainfoReady,
		PieceLength:   view.PieceLength,
		Pieces:        make([]pieceStatusResponse, len(view.Pieces)),
		Files:         make([]fileStatusResponse, len(view.Files)),
	}
	for i, piece := range view.Pieces {
		out.Pieces[i] = pieceStatusResponse{
			Index:          piece.Index,
			Known:          piece.Known,
			Complete:       piece.Complete,
			Partial:        piece.Partial,
			Wanted:         piece.Wanted,
			Checking:       piece.Checking,
			AvailableBytes: piece.AvailableBytes,
		}
	}
	for i, file := range view.Files {
		out.Files[i] = fileStatusResponse{
			Path:       file.Path,
			Size:       file.Size,
			PieceStart: file.PieceStart,
			PieceEnd:   file.PieceEnd,
		}
	}
	return out
}

func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing or invalid Content-Type")
		return
	}

	var src session.Source
	switch mediaType {
	case "application/json":
		r.Body = http.MaxBytesReader(w, r.Body, s.maxUpload)
		var body struct {
			MagnetURI string `json:"magnet_uri"`
		}
		if err := decodeJSON(r.Body, &body); err != nil {
			if isTooLarge(err) {
				writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if strings.TrimSpace(body.MagnetURI) == "" {
			writeError(w, http.StatusBadRequest, "magnet_uri is required")
			return
		}
		src = session.Source{MagnetURI: body.MagnetURI}
	case "multipart/form-data":
		r.Body = http.MaxBytesReader(w, r.Body, s.maxUpload)
		if err := r.ParseMultipartForm(s.maxUpload); err != nil {
			if isTooLarge(err) {
				writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			writeError(w, http.StatusBadRequest, "invalid multipart form")
			return
		}
		data, err := readUploadedTorrent(r)
		if err != nil {
			if isTooLarge(err) {
				writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			writeError(w, http.StatusBadRequest, "missing .torrent file")
			return
		}
		src = session.Source{Metainfo: data}
	default:
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json or multipart/form-data")
		return
	}

	view, err := s.backend.AddTorrentAndPersist(r.Context(), src)
	if err != nil {
		writeSessionError(w, "add", err)
		return
	}
	writeJSON(w, http.StatusCreated, newTorrentResponse(*view))
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	views := s.backend.ListTorrents()
	out := make([]torrentResponse, 0, len(views))
	for _, view := range views {
		out = append(out, newTorrentResponse(view))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	view, err := s.backend.TorrentViewFor(r.PathValue("id"))
	if err != nil {
		writeSessionError(w, "get torrent", err)
		return
	}
	writeJSON(w, http.StatusOK, newTorrentResponse(view))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	view, err := s.backend.TorrentStatusFor(r.PathValue("id"))
	if err != nil {
		writeSessionError(w, "get torrent status", err)
		return
	}
	writeJSON(w, http.StatusOK, newTorrentStatusResponse(view))
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	purge := false
	if raw := r.URL.Query().Get("purge_data"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "purge_data must be a boolean")
			return
		}
		purge = value
	}
	op, err := s.backend.DeleteTorrent(r.Context(), r.PathValue("id"), purge)
	if err != nil {
		writeSessionError(w, "delete", err)
		return
	}
	writeJSON(w, http.StatusAccepted, operationResponse{
		OperationID: op.ID,
		TorrentID:   op.TorrentID,
		State:       string(op.State),
		PurgeData:   op.PurgeData,
		Error:       op.Error,
	})
}

func (s *Server) handleOperation(w http.ResponseWriter, r *http.Request) {
	op, ok := s.backend.Operation(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown operation")
		return
	}
	writeJSON(w, http.StatusOK, operationResponse{
		OperationID: op.ID,
		TorrentID:   op.TorrentID,
		State:       string(op.State),
		PurgeData:   op.PurgeData,
		Error:       op.Error,
	})
}

func readUploadedTorrent(r *http.Request) ([]byte, error) {
	file, _, err := r.FormFile("file")
	if err != nil {
		// Accept a file part under a different field name as a fallback so
		// simple clients can post the .torrent as the only part.
		if r.MultipartForm == nil {
			return nil, err
		}
		for _, headers := range r.MultipartForm.File {
			if len(headers) == 0 {
				continue
			}
			f, err := headers[0].Open()
			if err != nil {
				return nil, err
			}
			defer func() { _ = f.Close() }()
			return io.ReadAll(f)
		}
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(file)
}

func writeSessionError(w http.ResponseWriter, action string, err error) {
	switch {
	case errors.Is(err, session.ErrDeleting):
		writeError(w, http.StatusConflict, "torrent is being deleted")
	case errors.Is(err, session.ErrExternalReference):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, session.ErrUnknownTorrent), errors.Is(err, filesystem.ErrNotFound):
		writeError(w, http.StatusNotFound, "unknown torrent")
	case errors.Is(err, session.ErrInvalidSource), errors.Is(err, filesystem.ErrInvalidName):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, action+" failed")
	}
}

func isTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}
