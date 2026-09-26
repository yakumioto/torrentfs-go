package api

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

type torrentResponse struct {
	ID              string    `json:"id"`
	InfoHash        string    `json:"info_hash"`
	Name            string    `json:"name"`
	State           string    `json:"state"`
	TotalBytes      int64     `json:"total_bytes"`
	DownloadedBytes int64     `json:"downloaded_bytes"`
	UploadedBytes   int64     `json:"uploaded_bytes"`
	CachedBytes     int64     `json:"cached_bytes"`
	CreatedAt       time.Time `json:"created_at"`
	Favorite        bool      `json:"favorite"`
	Error           string    `json:"error,omitempty"`
}

// maxOlderThanDays bounds the prune window. time.Duration is int64
// nanoseconds, so multiplying a day count above this overflows before the
// session ever sees it; a wrapped value would prune using a far smaller cutoff
// than the client asked for.
const maxOlderThanDays = 106751

type pruneFailureResponse struct {
	TorrentID string `json:"torrent_id"`
	Error     string `json:"error"`
}

type pruneResponse struct {
	Operations        []operationResponse `json:"operations"`
	ExcludedFavorites int                 `json:"excluded_favorites"`
	// Failures is omitted when every candidate started deleting, so a clean run
	// and a run that could not start some deletions never have the same shape.
	Failures []pruneFailureResponse `json:"failures,omitempty"`
}

type operationResponse struct {
	OperationID string `json:"operation_id"`
	TorrentID   string `json:"torrent_id"`
	State       string `json:"state"`
	Error       string `json:"error,omitempty"`
}

type runtimeStatsResponse struct {
	StartedAt time.Time               `json:"started_at"`
	Cache     runtimeCacheResponse    `json:"cache"`
	Transfer  runtimeTransferResponse `json:"transfer"`
}

type runtimeCacheResponse struct {
	UsedBytes     int64 `json:"used_bytes"`
	CapacityBytes int64 `json:"capacity_bytes"`
}

type runtimeTransferResponse struct {
	DownloadedBytes int64 `json:"downloaded_bytes"`
	UploadedBytes   int64 `json:"uploaded_bytes"`
}

type pieceStatusResponse struct {
	Index       int   `json:"index"`
	Cached      bool  `json:"cached"`
	CachedBytes int64 `json:"cached_bytes"`
	Pinned      bool  `json:"pinned"`
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
	Network       networkStatusResponse `json:"network"`
}

// networkStatusResponse reports network visibility next to the piece data.
// Every field is additive; metainfo_ready keeps its original meaning.
type networkStatusResponse struct {
	EffectiveListenPort int                       `json:"effective_listen_port"`
	TotalPeers          int                       `json:"total_peers"`
	PendingPeers        int                       `json:"pending_peers"`
	ActivePeers         int                       `json:"active_peers"`
	ConnectedSeeders    int                       `json:"connected_seeders"`
	PieceComplete       int                       `json:"piece_complete"`
	Dht                 []dhtFamilyStatusResponse `json:"dht"`
}

type dhtFamilyStatusResponse struct {
	Family    string `json:"family"`
	LocalAddr string `json:"local_addr"`
	Nodes     int    `json:"nodes"`
	GoodNodes int    `json:"good_nodes"`
	Resolved  int    `json:"resolved"`
	Kept      int    `json:"kept"`
	Ready     bool   `json:"ready"`
	Error     string `json:"error,omitempty"`
}

func newTorrentResponse(view session.TorrentView) torrentResponse {
	return torrentResponse{
		ID:              view.ID,
		InfoHash:        view.InfoHash,
		Name:            view.Name,
		State:           string(view.State),
		TotalBytes:      view.TotalBytes,
		DownloadedBytes: view.DownloadedBytes,
		UploadedBytes:   view.UploadedBytes,
		CachedBytes:     view.CachedBytes,
		CreatedAt:       view.CreatedAt,
		Favorite:        view.Favorite,
		Error:           view.Error,
	}
}

func newRuntimeStatsResponse(view session.RuntimeStatsView) runtimeStatsResponse {
	return runtimeStatsResponse{
		StartedAt: view.StartedAt,
		Cache: runtimeCacheResponse{
			UsedBytes:     view.CacheUsedBytes,
			CapacityBytes: view.CacheCapacityBytes,
		},
		Transfer: runtimeTransferResponse{
			DownloadedBytes: view.DownloadedBytes,
			UploadedBytes:   view.UploadedBytes,
		},
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
			Index:       piece.Index,
			Cached:      piece.Cached,
			CachedBytes: piece.CachedBytes,
			Pinned:      piece.Pinned,
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
	out.Network = newNetworkStatusResponse(view.Network)
	return out
}

func newNetworkStatusResponse(view session.NetworkStatus) networkStatusResponse {
	out := networkStatusResponse{
		EffectiveListenPort: view.EffectiveListenPort,
		TotalPeers:          view.TotalPeers,
		PendingPeers:        view.PendingPeers,
		ActivePeers:         view.ActivePeers,
		ConnectedSeeders:    view.ConnectedSeeders,
		PieceComplete:       view.PiecesComplete,
		Dht:                 make([]dhtFamilyStatusResponse, len(view.DhtFamilies)),
	}
	for i, family := range view.DhtFamilies {
		out.Dht[i] = dhtFamilyStatusResponse{
			Family:    family.Family,
			LocalAddr: family.LocalAddr,
			Nodes:     family.Nodes,
			GoodNodes: family.GoodNodes,
			Resolved:  family.Resolved,
			Kept:      family.Kept,
			Ready:     family.Ready,
			Error:     family.Error,
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

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, newRuntimeStatsResponse(s.backend.RuntimeStats()))
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
	op, err := s.backend.DeleteTorrent(r.Context(), r.PathValue("id"))
	if err != nil {
		writeSessionError(w, "delete", err)
		return
	}
	writeJSON(w, http.StatusAccepted, operationResponse{
		OperationID: op.ID,
		TorrentID:   op.TorrentID,
		State:       string(op.State),
		Error:       op.Error,
	})
}

func (s *Server) handleSetFavorite(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUpload)
	var body struct {
		Favorite *bool `json:"favorite"`
	}
	if err := decodeJSON(r.Body, &body); err != nil {
		if isTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Favorite == nil {
		writeError(w, http.StatusBadRequest, "favorite is required")
		return
	}
	view, err := s.backend.SetFavorite(r.Context(), r.PathValue("id"), *body.Favorite)
	if err != nil {
		writeSessionError(w, "set favorite", err)
		return
	}
	writeJSON(w, http.StatusOK, newTorrentResponse(view))
}

func (s *Server) handlePrune(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUpload)
	var body struct {
		OlderThanDays *int `json:"older_than_days"`
	}
	if err := decodeJSON(r.Body, &body); err != nil {
		if isTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.OlderThanDays == nil || *body.OlderThanDays < 1 || *body.OlderThanDays > maxOlderThanDays {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("older_than_days must be an integer between 1 and %d", maxOlderThanDays))
		return
	}
	result, err := s.backend.DeleteUnfavoritedOlderThan(r.Context(), time.Duration(*body.OlderThanDays)*24*time.Hour)
	if err != nil {
		writeSessionError(w, "prune", err)
		return
	}
	out := pruneResponse{
		Operations:        make([]operationResponse, 0, len(result.Operations)),
		ExcludedFavorites: result.ExcludedFavorites,
	}
	for _, op := range result.Operations {
		out.Operations = append(out.Operations, operationResponse{
			OperationID: op.ID,
			TorrentID:   op.TorrentID,
			State:       string(op.State),
			Error:       op.Error,
		})
	}
	for _, failure := range result.Failures {
		out.Failures = append(out.Failures, pruneFailureResponse{
			TorrentID: failure.TorrentID,
			Error:     failure.Error,
		})
	}
	writeJSON(w, http.StatusOK, out)
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
