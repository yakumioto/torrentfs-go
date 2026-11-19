package api

import (
	"errors"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

type subtitleTargetResponse struct {
	VideoPath        string `json:"video_path"`
	MountPath        string `json:"mount_path"`
	ExpectedBasename string `json:"expected_basename"`
	Uploadable       bool   `json:"uploadable"`
	Reason           string `json:"reason,omitempty"`
}

type subtitleResponse struct {
	VideoPath string    `json:"video_path"`
	Path      string    `json:"path"`
	MountPath string    `json:"mount_path"`
	Format    string    `json:"format"`
	Size      int64     `json:"size"`
	UpdatedAt time.Time `json:"updated_at"`
}

type subtitleUploadResponse struct {
	TorrentID string    `json:"torrent_id"`
	VideoPath string    `json:"video_path"`
	Path      string    `json:"path"`
	MountPath string    `json:"mount_path"`
	Format    string    `json:"format"`
	Size      int64     `json:"size"`
	UpdatedAt time.Time `json:"updated_at"`
	Replaced  bool      `json:"replaced"`
}

func newSubtitleTargetResponses(targets []session.SubtitleTarget) []subtitleTargetResponse {
	out := make([]subtitleTargetResponse, 0, len(targets))
	for _, target := range targets {
		out = append(out, subtitleTargetResponse{
			VideoPath:        target.VideoPath,
			MountPath:        target.MountPath,
			ExpectedBasename: target.ExpectedBasename,
			Uploadable:       target.Uploadable,
			Reason:           target.Reason,
		})
	}
	return out
}

func newSubtitleResponses(subtitles []session.Subtitle) []subtitleResponse {
	out := make([]subtitleResponse, 0, len(subtitles))
	for _, subtitle := range subtitles {
		out = append(out, subtitleResponse{
			VideoPath: subtitle.VideoPath,
			Path:      subtitle.Path,
			MountPath: subtitle.MountPath,
			Format:    subtitle.Format,
			Size:      subtitle.Size,
			UpdatedAt: subtitle.UpdatedAt,
		})
	}
	return out
}

func (s *Server) handleUploadSubtitle(w http.ResponseWriter, r *http.Request) {
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "multipart/form-data" {
		writeError(w, http.StatusBadRequest, "Content-Type must be multipart/form-data")
		return
	}

	limit := s.maxUploadBytes()
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseMultipartForm(limit); err != nil {
		if isTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid multipart form")
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	videoPath := strings.TrimSpace(r.FormValue("video_path"))
	if videoPath == "" {
		writeError(w, http.StatusBadRequest, "video_path is required")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "file is required")
		return
	}
	defer func() { _ = file.Close() }()

	result, err := s.backend.UploadSubtitle(r.Context(), r.PathValue("id"), videoPath, header.Filename, file, limit)
	if err != nil {
		writeSubtitleError(w, "upload subtitle", err)
		return
	}
	status := http.StatusCreated
	if result.Replaced {
		status = http.StatusOK
	}
	writeJSON(w, status, subtitleUploadResponse{
		TorrentID: result.TorrentID,
		VideoPath: result.VideoPath,
		Path:      result.Path,
		MountPath: result.MountPath,
		Format:    result.Format,
		Size:      result.Size,
		UpdatedAt: result.UpdatedAt,
		Replaced:  result.Replaced,
	})
}

func (s *Server) handleDeleteSubtitle(w http.ResponseWriter, r *http.Request) {
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	if err := s.backend.DeleteSubtitle(r.Context(), r.PathValue("id"), relPath); err != nil {
		writeSubtitleError(w, "delete subtitle", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeSubtitleError maps a subtitle failure onto a status and a stable code.
// The code is what the web client branches on; the message stays human text.
// The action names the endpoint that failed, so a 500 does not claim an upload
// failed when a delete did.
func writeSubtitleError(w http.ResponseWriter, action string, err error) {
	code := session.SubtitleErrorCode(err)
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, session.ErrUnknownTorrent):
		status = http.StatusNotFound
	case errors.Is(err, session.ErrDeleting) || code == session.SubtitleCodeTorrentDeleting:
		status = http.StatusConflict
		code = codeOr(code, session.SubtitleCodeTorrentDeleting)
	case errors.Is(err, session.ErrSubtitleNotFound) || code == session.SubtitleCodeNotFound:
		status = http.StatusNotFound
		code = codeOr(code, session.SubtitleCodeNotFound)
	case errors.Is(err, session.ErrSubtitleVideoNotFound) || code == session.SubtitleCodeVideoNotFound:
		status = http.StatusNotFound
		code = codeOr(code, session.SubtitleCodeVideoNotFound)
	case errors.Is(err, session.ErrSubtitleNameMismatch) || code == session.SubtitleCodeNameMismatch:
		status = http.StatusUnsupportedMediaType
		code = codeOr(code, session.SubtitleCodeNameMismatch)
	case errors.Is(err, session.ErrSubtitleFormatUnsupported) || code == session.SubtitleCodeFormatUnsupported:
		status = http.StatusUnsupportedMediaType
		code = codeOr(code, session.SubtitleCodeFormatUnsupported)
	case errors.Is(err, session.ErrSubtitleNameConflict) || code == session.SubtitleCodeNameConflict:
		status = http.StatusConflict
		code = codeOr(code, session.SubtitleCodeNameConflict)
	case errors.Is(err, session.ErrSubtitlePayloadConflict) || code == session.SubtitleCodePayloadConflict:
		status = http.StatusConflict
		code = codeOr(code, session.SubtitleCodePayloadConflict)
	case errors.Is(err, session.ErrSubtitleUploadTooLarge) || code == session.SubtitleCodeUploadTooLarge:
		status = http.StatusRequestEntityTooLarge
		code = codeOr(code, session.SubtitleCodeUploadTooLarge)
	// The store wraps the underlying filesystem error, so the code is the only
	// reliable signal: the sentinel itself is not in the chain.
	case errors.Is(err, session.ErrSubtitleStorageFull) || code == session.SubtitleCodeStorageFull:
		status = http.StatusInsufficientStorage
		code = codeOr(code, session.SubtitleCodeStorageFull)
	case errors.Is(err, session.ErrSubtitleStorageUnavailable) || code == session.SubtitleCodeStorageUnavailable:
		status = http.StatusServiceUnavailable
		code = codeOr(code, session.SubtitleCodeStorageUnavailable)
	case errors.Is(err, session.ErrSubtitleCleanupFailed) || code == session.SubtitleCodeCleanupFailed:
		status = http.StatusInternalServerError
		code = codeOr(code, session.SubtitleCodeCleanupFailed)
	}
	if code == "" {
		writeSessionError(w, action, err)
		return
	}
	writeCodedError(w, status, code, err.Error())
}

func codeOr(code, fallback string) string {
	if code != "" {
		return code
	}
	return fallback
}

func writeCodedError(w http.ResponseWriter, status int, code, message string) {
	body := map[string]string{"error": message}
	if code != "" {
		body["code"] = code
	}
	writeJSON(w, status, body)
}
