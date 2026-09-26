package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

func subtitleRequest(t *testing.T, id, videoPath, fileName, content string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if videoPath != "" {
		if err := writer.WriteField("video_path", videoPath); err != nil {
			t.Fatalf("write video_path: %v", err)
		}
	}
	if fileName != "" {
		part, err := writer.CreateFormFile("file", fileName)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := part.Write([]byte(content)); err != nil {
			t.Fatalf("write subtitle part: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/torrents/"+id+"/subtitles", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func TestUploadSubtitleMultipartContract(t *testing.T) {
	id := strings.Repeat("a", 40)
	backend := &fakeBackend{subtitleUploadResponse: session.SubtitleUploadResponse{
		TorrentID: id,
		VideoPath: "Season 1/E01.mp4",
		Path:      "Season 1/E01.srt",
		MountPath: "Show/Season 1/E01.srt",
		Format:    "srt",
		Size:      7,
		UpdatedAt: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
	}}
	srv := newTestServer(t, backend, nil)

	rec := do(t, srv, subtitleRequest(t, id, "Season 1/E01.mp4", "E01.srt", "subtitle"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	if backend.subtitleID != id || backend.subtitleVideoPath != "Season 1/E01.mp4" || backend.subtitleFileName != "E01.srt" {
		t.Fatalf("backend call = (%q, %q, %q)", backend.subtitleID, backend.subtitleVideoPath, backend.subtitleFileName)
	}
	if string(backend.subtitleBody) != "subtitle" {
		t.Fatalf("backend body = %q, want the uploaded bytes", backend.subtitleBody)
	}
	if backend.subtitleMaxBytes <= 0 {
		t.Fatalf("backend size cap = %d, want the configured upload limit", backend.subtitleMaxBytes)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["torrent_id"] != id || got["path"] != "Season 1/E01.srt" || got["mount_path"] != "Show/Season 1/E01.srt" ||
		got["format"] != "srt" || got["replaced"] != false {
		t.Fatalf("response = %v, want the subtitle descriptor", got)
	}
}

func TestUploadSubtitleReplacementReturnsOK(t *testing.T) {
	id := strings.Repeat("b", 40)
	backend := &fakeBackend{subtitleUploadResponse: session.SubtitleUploadResponse{
		TorrentID: id,
		VideoPath: "Movie.2026.mkv",
		Path:      "Movie.2026.srt",
		MountPath: "Show/Movie.2026.srt",
		Format:    "srt",
		Size:      3,
		UpdatedAt: time.Now().UTC(),
		Replaced:  true,
	}}
	srv := newTestServer(t, backend, nil)

	rec := do(t, srv, subtitleRequest(t, id, "Movie.2026.mkv", "Movie.2026.srt", "new"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a replacement; body %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["replaced"] != true {
		t.Fatalf("response = %v, want replaced=true", got)
	}
}

func TestUploadSubtitleRejectsBadRequests(t *testing.T) {
	id := strings.Repeat("c", 40)
	tests := []struct {
		name      string
		videoPath string
		fileName  string
		want      int
	}{
		{"missing video path", "", "E01.srt", http.StatusBadRequest},
		{"missing file", "E01.mp4", "", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t, &fakeBackend{}, nil)
			rec := do(t, srv, subtitleRequest(t, id, tt.videoPath, tt.fileName, "x"))
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.want, rec.Body.String())
			}
		})
	}

	srv := newTestServer(t, &fakeBackend{}, nil)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/torrents/"+id+"/subtitles", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	if rec := do(t, srv, req); rec.Code != http.StatusBadRequest {
		t.Fatalf("json status = %d, want 400; body %s", rec.Code, rec.Body.String())
	}

	// The upload limit is shared with the .torrent endpoint.
	small := newTestServer(t, &fakeBackend{}, nil)
	oversize := subtitleRequest(t, id, "E01.mp4", "E01.srt", strings.Repeat("x", 64))
	small = newTestServer(t, &fakeBackend{}, func(cfg *config.Config) { cfg.HTTP.MaxUploadBytes = 8 })
	if rec := do(t, small, oversize); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d, want 413; body %s", rec.Code, rec.Body.String())
	}
}

func TestUploadSubtitleMapsErrorsToStatus(t *testing.T) {
	id := strings.Repeat("d", 40)
	tests := []struct {
		name   string
		cause  error
		status int
	}{
		{"name mismatch", session.ErrSubtitleNameMismatch, http.StatusUnsupportedMediaType},
		{"format unsupported", session.ErrSubtitleFormatUnsupported, http.StatusUnsupportedMediaType},
		{"video not found", session.ErrSubtitleVideoNotFound, http.StatusNotFound},
		{"name conflict", session.ErrSubtitleNameConflict, http.StatusConflict},
		{"payload conflict", session.ErrSubtitlePayloadConflict, http.StatusConflict},
		{"storage unavailable", session.ErrSubtitleStorageUnavailable, http.StatusServiceUnavailable},
		{"storage full", session.ErrSubtitleStorageFull, http.StatusInsufficientStorage},
		{"write failed", session.ErrSubtitleWriteFailed, http.StatusInternalServerError},
		{"upload too large", session.ErrSubtitleUploadTooLarge, http.StatusRequestEntityTooLarge},
		{"deleting", session.ErrDeleting, http.StatusConflict},
		{"unknown torrent", session.ErrUnknownTorrent, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t, &fakeBackend{subtitleErr: tt.cause}, nil)
			rec := do(t, srv, subtitleRequest(t, id, "E01.mp4", "E01.srt", "x"))
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.status, rec.Body.String())
			}
			// A failure response must never echo a filesystem path or the
			// uploaded bytes back to the client.
			if strings.Contains(rec.Body.String(), "/") && tt.cause == session.ErrSubtitleStorageUnavailable {
				t.Fatalf("storage error body leaked a path: %s", rec.Body.String())
			}
		})
	}
}

func TestSubtitleUploadRequiresAuthentication(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, dynamicAuth)
	rec := do(t, srv, subtitleRequest(t, strings.Repeat("e", 40), "E01.mp4", "E01.srt", "x"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated subtitle upload status = %d, want 401", rec.Code)
	}
}

func TestStatusExposesTargetsAndSubtitles(t *testing.T) {
	id := strings.Repeat("f", 40)
	backend := &fakeBackend{statusView: session.TorrentStatusView{
		Torrent:       session.TorrentView{ID: id, InfoHash: id, State: session.StateReady},
		MetainfoReady: true,
		PieceLength:   64,
		Pieces:        []session.PieceStatus{},
		Files:         []session.FileStatus{{Path: "Movie.2026.mkv", Size: 10, PieceStart: 0, PieceEnd: 1}},
		SubtitleTargets: []session.SubtitleTarget{{
			VideoPath:        "Movie.2026.mkv",
			MountPath:        "Show/Movie.2026.srt",
			ExpectedBasename: "Movie.2026",
			Uploadable:       true,
		}, {
			VideoPath:  "other.mkv",
			MountPath:  "Show/other.srt",
			Uploadable: false,
			Reason:     session.SubtitleCodeNameConflict,
		}},
		Subtitles: []session.Subtitle{{
			TorrentID: id,
			VideoPath: "Movie.2026.mkv",
			Path:      "Movie.2026.srt",
			MountPath: "Show/Movie.2026.srt",
			Format:    "srt",
			Size:      7,
		}},
	}}
	srv := newTestServer(t, backend, nil)

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/torrents/"+id+"/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var got struct {
		SubtitleTargets []struct {
			VideoPath        string `json:"video_path"`
			MountPath        string `json:"mount_path"`
			ExpectedBasename string `json:"expected_basename"`
			Uploadable       bool   `json:"uploadable"`
			Reason           string `json:"reason"`
		} `json:"subtitle_targets"`
		Subtitles []struct {
			VideoPath string `json:"video_path"`
			Path      string `json:"path"`
			MountPath string `json:"mount_path"`
			Format    string `json:"format"`
		} `json:"subtitles"`
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if len(got.SubtitleTargets) != 2 || got.SubtitleTargets[0].ExpectedBasename != "Movie.2026" || !got.SubtitleTargets[0].Uploadable {
		t.Fatalf("subtitle_targets = %+v", got.SubtitleTargets)
	}
	if got.SubtitleTargets[1].Uploadable || got.SubtitleTargets[1].Reason != session.SubtitleCodeNameConflict {
		t.Fatalf("blocked target = %+v, want an uploadable=false conflict", got.SubtitleTargets[1])
	}
	if len(got.Subtitles) != 1 || got.Subtitles[0].Path != "Movie.2026.srt" || got.Subtitles[0].Format != "srt" {
		t.Fatalf("subtitles = %+v", got.Subtitles)
	}
	// The payload list keeps its piece-range meaning: no subtitle leaks into it.
	if len(got.Files) != 1 || got.Files[0].Path != "Movie.2026.mkv" {
		t.Fatalf("files = %+v, want only the payload file", got.Files)
	}
}

func TestStatusWithoutMetainfoKeepsSubtitleArraysEmpty(t *testing.T) {
	id := strings.Repeat("g", 40)
	backend := &fakeBackend{statusView: session.TorrentStatusView{
		Torrent: session.TorrentView{ID: id, InfoHash: id, State: session.StateAdding},
		Pieces:  []session.PieceStatus{},
		Files:   []session.FileStatus{},
	}}
	srv := newTestServer(t, backend, nil)

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/torrents/"+id+"/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	for _, key := range []string{"subtitle_targets", "subtitles"} {
		value, ok := got[key].([]any)
		if !ok || len(value) != 0 {
			t.Fatalf("%s = %v, want []", key, got[key])
		}
	}
}

func TestOperationExposesCleanupErrorCode(t *testing.T) {
	backend := &fakeBackend{ops: map[string]session.Operation{
		"op-1": {
			ID:        "op-1",
			TorrentID: strings.Repeat("h", 40),
			State:     session.StateDeleteFailed,
			Error:     "subtitle cleanup failed",
			ErrorCode: session.SubtitleCodeCleanupFailed,
		},
	}}
	srv := newTestServer(t, backend, nil)

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/operations/op-1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode operation: %v", err)
	}
	if got["state"] != "delete_failed" || got["error_code"] != session.SubtitleCodeCleanupFailed {
		t.Fatalf("operation = %v, want delete_failed with the cleanup code", got)
	}

	// A healthy operation must not carry the key at all.
	healthy := &fakeBackend{deleteOp: &session.Operation{ID: "op-2", TorrentID: strings.Repeat("h", 40), State: session.StateDeleting}}
	empty := newTestServer(t, healthy, nil)
	rec = do(t, empty, httptest.NewRequest(http.MethodDelete, "/api/v1/torrents/"+strings.Repeat("h", 40), nil))
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode delete: %v", err)
	}
	if _, ok := raw["error_code"]; ok {
		t.Fatalf("healthy operation exposed error_code: %v", raw)
	}
}

// TestUploadSubtitleEndToEndCarriesCodes drives the real session so the codes
// the web client branches on are proven to reach the wire: a fake backend can
// only show that the handler maps error values, not that the session produces
// them.
func TestUploadSubtitleEndToEndCarriesCodes(t *testing.T) {
	torrentsDir := filepath.Join(t.TempDir(), "torrents")
	if err := os.MkdirAll(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	cfg := config.Default()
	sess, err := session.New(cfg, torrentsDir)
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	srv, err := api.New(cfg, sess)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	torrentBytes := buildTestTorrent(t, "Movie.2026.mkv", []byte("video payload"))
	var add bytes.Buffer
	writer := multipart.NewWriter(&add)
	part, err := writer.CreateFormFile("file", "payload.torrent")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(torrentBytes); err != nil {
		t.Fatalf("write torrent part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	addReq := httptest.NewRequest(http.MethodPost, "/api/v1/torrents", &add)
	addReq.Header.Set("Content-Type", writer.FormDataContentType())
	addRec := do(t, srv, addReq)
	if addRec.Code != http.StatusCreated {
		t.Fatalf("add status = %d, want 201; body %s", addRec.Code, addRec.Body.String())
	}
	var added struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(addRec.Body.Bytes(), &added); err != nil {
		t.Fatalf("decode add: %v", err)
	}

	statusRec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/torrents/"+added.ID+"/status", nil))
	var status struct {
		SubtitleTargets []struct {
			VideoPath  string `json:"video_path"`
			MountPath  string `json:"mount_path"`
			Uploadable bool   `json:"uploadable"`
		} `json:"subtitle_targets"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if len(status.SubtitleTargets) != 1 || !status.SubtitleTargets[0].Uploadable {
		t.Fatalf("subtitle_targets = %+v, want one uploadable target", status.SubtitleTargets)
	}
	if status.SubtitleTargets[0].MountPath != "Movie.2026.srt" {
		t.Fatalf("mount path = %q, want Movie.2026.srt beside a single-file video", status.SubtitleTargets[0].MountPath)
	}

	created := do(t, srv, subtitleRequest(t, added.ID, "Movie.2026.mkv", "Movie.2026.srt", "subtitle body\n"))
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body %s", created.Code, created.Body.String())
	}
	replaced := do(t, srv, subtitleRequest(t, added.ID, "Movie.2026.mkv", "Movie.2026.srt", "new body\n"))
	if replaced.Code != http.StatusOK {
		t.Fatalf("replace status = %d, want 200; body %s", replaced.Code, replaced.Body.String())
	}
	var replacedBody map[string]any
	if err := json.Unmarshal(replaced.Body.Bytes(), &replacedBody); err != nil {
		t.Fatalf("decode replace: %v", err)
	}
	if replacedBody["replaced"] != true || replacedBody["size"] != float64(len("new body\n")) {
		t.Fatalf("replace body = %v, want the new size and replaced=true", replacedBody)
	}

	// The status array is what a refreshed page shows, so it must report the
	// replacement too.
	statusRec = do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/torrents/"+added.ID+"/status", nil))
	var after struct {
		Subtitles []struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		} `json:"subtitles"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode status after upload: %v", err)
	}
	if len(after.Subtitles) != 1 || after.Subtitles[0].Size != int64(len("new body\n")) {
		t.Fatalf("subtitles after replace = %+v, want the new size", after.Subtitles)
	}

	for _, tt := range []struct {
		name      string
		videoPath string
		fileName  string
		status    int
		code      string
	}{
		{"name mismatch", "Movie.2026.mkv", "other.srt", http.StatusUnsupportedMediaType, session.SubtitleCodeNameMismatch},
		{"uppercase extension", "Movie.2026.mkv", "Movie.2026.SRT", http.StatusUnsupportedMediaType, session.SubtitleCodeFormatUnsupported},
		{"unknown video", "missing.mkv", "missing.srt", http.StatusNotFound, session.SubtitleCodeVideoNotFound},
		{"traversal video path", "../Movie.2026.mkv", "Movie.2026.srt", http.StatusNotFound, session.SubtitleCodeVideoNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, srv, subtitleRequest(t, added.ID, tt.videoPath, tt.fileName, "x"))
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.status, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if body["code"] != tt.code {
				t.Fatalf("error body = %v, want code %s", body, tt.code)
			}
		})
	}

	// Deleting the torrent removes the subtitle the API just wrote.
	deleteRec := do(t, srv, httptest.NewRequest(http.MethodDelete, "/api/v1/torrents/"+added.ID, nil))
	if deleteRec.Code != http.StatusAccepted {
		t.Fatalf("delete status = %d, want 202; body %s", deleteRec.Code, deleteRec.Body.String())
	}
	var op struct {
		OperationID string `json:"operation_id"`
		State       string `json:"state"`
	}
	if err := json.Unmarshal(deleteRec.Body.Bytes(), &op); err != nil {
		t.Fatalf("decode delete: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for op.State == string(session.StateDeleting) {
		if time.Now().After(deadline) {
			t.Fatal("delete operation never finished")
		}
		time.Sleep(10 * time.Millisecond)
		rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/operations/"+op.OperationID, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("operation status = %d, want 200", rec.Code)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &op); err != nil {
			t.Fatalf("decode operation: %v", err)
		}
	}
	if op.State != string(session.StateDeleted) {
		t.Fatalf("operation state = %s, want deleted", op.State)
	}
	subtitlesDir := filepath.Join(torrentsDir, ".metadata", "subtitles", added.ID)
	if _, err := os.Stat(subtitlesDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("subtitle store survived the delete: %v", err)
	}
}
