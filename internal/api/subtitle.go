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

	r.Body = http.MaxBytesReader(w, r.Body, s.maxUpload)
	if err := r.ParseMultipartForm(s.maxUpload); err != nil {
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

	result, err := s.backend.UploadSubtitle(r.Context(), r.PathValue("id"), videoPath, header.Filename, file, s.maxUpload)
	if err != nil {
		writeSubtitleError(w, err)
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

// writeSubtitleError maps a subtitle failure onto a status and a stable code.
// The code is what the web client branches on; the message stays human text.
func writeSubtitleError(w http.ResponseWriter, err error) {
	code := session.SubtitleErrorCode(err)
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, session.ErrDeleting) || code == session.SubtitleCodeTorrentDeleting:
		status = http.StatusConflict
	case errors.Is(err, session.ErrUnknownTorrent):
		status = http.StatusNotFound
	case errors.Is(err, session.ErrSubtitleNameMismatch), errors.Is(err, session.ErrSubtitleFormatUnsupported):
		status = http.StatusUnsupportedMediaType
	case errors.Is(err, session.ErrSubtitleVideoNotFound):
		status = http.StatusNotFound
	case errors.Is(err, session.ErrSubtitleNameConflict), errors.Is(err, session.ErrSubtitlePayloadConflict):
		status = http.StatusConflict
	case errors.Is(err, session.ErrSubtitleUploadTooLarge):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, session.ErrSubtitleStorageFull):
		status = http.StatusInsufficientStorage
	case errors.Is(err, session.ErrSubtitleStorageUnavailable):
		status = http.StatusServiceUnavailable
	case errors.Is(err, session.ErrSubtitleWriteFailed):
		status = http.StatusInternalServerError
	}
	if status == http.StatusInternalServerError {
		writeSessionError(w, "upload subtitle", err)
		return
	}
	writeCodedError(w, status, code, err.Error())
}

func writeCodedError(w http.ResponseWriter, status int, code, message string) {
	body := map[string]string{"error": message}
	if code != "" {
		body["code"] = code
	}
	writeJSON(w, status, body)
}
