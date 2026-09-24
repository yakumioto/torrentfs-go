package api

import (
	"errors"
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
	Error           string    `json:"error,omitempty"`
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
	Torrent       torrentResponse          `json:"torrent"`
	MetainfoReady bool                     `json:"metainfo_ready"`
	PieceLength   int64                    `json:"piece_length"`
	Pieces        []pieceStatusResponse    `json:"pieces"`
	Files         []fileStatusResponse     `json:"files"`
	Playback      []playbackStreamResponse `json:"playback_streams"`
	Network       networkStatusResponse    `json:"network"`
}

type playbackStreamRequest struct {
	Path          string `json:"path"`
	PositionBytes int64  `json:"position_bytes"`
}

type playbackUpdateRequest struct {
	Sequence      uint64 `json:"sequence"`
	Event         string `json:"event"`
	PositionBytes int64  `json:"position_bytes"`
}

type playbackStreamResponse struct {
	ID                    string    `json:"stream_id"`
	TorrentID             string    `json:"torrent_id"`
	Path                  string    `json:"path"`
	Generation            uint64    `json:"generation"`
	PlaybackCursor        int64     `json:"playback_cursor"`
	PlaybackConsumedBytes int64     `json:"playback_consumed_bytes"`
	UsefulDownloadBytes   int64     `json:"useful_download_bytes"`
	CacheResidentBytes    int64     `json:"cache_resident_bytes"`
	BufferedBytes         int64     `json:"buffered_bytes"`
	TargetLow             int64     `json:"target_low_bytes"`
	TargetHigh            int64     `json:"target_high_bytes"`
	EffectiveLow          int64     `json:"effective_low_bytes"`
	EffectiveHigh         int64     `json:"effective_high_bytes"`
	State                 string    `json:"state"`
	LastSequence          uint64    `json:"last_sequence"`
	LastUpdate            time.Time `json:"last_update"`
	ExpiresAt             time.Time `json:"expires_at"`
	ForegroundPieces      []int     `json:"foreground_pieces"`
	BackgroundPieces      []int     `json:"background_pieces"`
	PinnedPieces          []int     `json:"pinned_pieces"`
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
	playback := view.Playback
	if len(playback) == 0 {
		playback = view.PlaybackStreams
	}
	out := torrentStatusResponse{
		Torrent:       newTorrentResponse(view.Torrent),
		MetainfoReady: view.MetainfoReady,
		PieceLength:   view.PieceLength,
		Pieces:        make([]pieceStatusResponse, len(view.Pieces)),
		Files:         make([]fileStatusResponse, len(view.Files)),
		Playback:      make([]playbackStreamResponse, len(playback)),
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
	for i, stream := range playback {
		out.Playback[i] = newPlaybackStreamResponse(stream)
	}
	out.Network = newNetworkStatusResponse(view.Network)
	return out
}

func newPlaybackStreamResponse(view session.PlaybackStreamSnapshot) playbackStreamResponse {
	return playbackStreamResponse{
		ID:                    view.ID,
		TorrentID:             view.TorrentID,
		Path:                  view.Path,
		Generation:            view.Generation,
		PlaybackCursor:        view.PlaybackCursor,
		PlaybackConsumedBytes: view.PlaybackConsumedBytes,
		UsefulDownloadBytes:   view.UsefulDownloadBytes,
		CacheResidentBytes:    view.CacheResidentBytes,
		BufferedBytes:         view.BufferedBytes,
		TargetLow:             view.TargetLow,
		TargetHigh:            view.TargetHigh,
		EffectiveLow:          view.EffectiveLow,
		EffectiveHigh:         view.EffectiveHigh,
		State:                 view.State,
		LastSequence:          view.LastSequence,
		LastUpdate:            view.LastUpdate,
		ExpiresAt:             view.ExpiresAt,
		ForegroundPieces:      append([]int(nil), view.ForegroundPieces...),
		BackgroundPieces:      append([]int(nil), view.BackgroundPieces...),
		PinnedPieces:          append([]int(nil), view.PinnedPieces...),
	}
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

func (s *Server) handlePlaybackStart(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.backend.(PlaybackBackend)
	if !ok {
		writeError(w, http.StatusNotImplemented, "playback control is unavailable")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body playbackStreamRequest
	if err := decodeJSON(r.Body, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid playback stream body")
		return
	}
	if strings.TrimSpace(body.Path) == "" || body.PositionBytes < 0 {
		writeError(w, http.StatusBadRequest, "path and non-negative position_bytes are required")
		return
	}
	snapshot, err := backend.StartPlaybackStream(r.Context(), r.PathValue("id"), session.PlaybackStreamStart{
		Path:          body.Path,
		PositionBytes: body.PositionBytes,
	})
	if err != nil {
		writePlaybackError(w, "start playback", err)
		return
	}
	writeJSON(w, http.StatusCreated, newPlaybackStreamResponse(snapshot))
}

func (s *Server) handlePlaybackUpdate(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.backend.(PlaybackBackend)
	if !ok {
		writeError(w, http.StatusNotImplemented, "playback control is unavailable")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body playbackUpdateRequest
	if err := decodeJSON(r.Body, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid playback update body")
		return
	}
	snapshot, err := backend.UpdatePlaybackStream(r.Context(), r.PathValue("stream_id"), session.PlaybackStreamUpdate{
		Sequence:      body.Sequence,
		Event:         body.Event,
		PositionBytes: body.PositionBytes,
	})
	if err != nil {
		writePlaybackError(w, "update playback", err)
		return
	}
	writeJSON(w, http.StatusOK, newPlaybackStreamResponse(snapshot))
}

func (s *Server) handlePlaybackStop(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.backend.(PlaybackBackend)
	if !ok {
		writeError(w, http.StatusNotImplemented, "playback control is unavailable")
		return
	}
	if err := backend.StopPlaybackStream(r.Context(), r.PathValue("stream_id")); err != nil {
		writePlaybackError(w, "stop playback", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writePlaybackError(w http.ResponseWriter, action string, err error) {
	switch {
	case errors.Is(err, session.ErrPlaybackStreamNotFound):
		writeError(w, http.StatusNotFound, "unknown playback stream")
	case errors.Is(err, session.ErrPlaybackSequenceConflict):
		writeError(w, http.StatusConflict, "playback sequence conflict")
	case errors.Is(err, session.ErrPlaybackEventInvalid), errors.Is(err, session.ErrPlaybackPositionInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeSessionError(w, action, err)
	}
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
