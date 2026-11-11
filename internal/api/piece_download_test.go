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

func TestStartPieceDownloadReturnsAccepted(t *testing.T) {
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	backend := &fakeBackend{pieceDownloadStatus: session.PieceDownloadStatus{
		State:           session.PieceDownloadRunning,
		RequestedPieces: 3,
		DonePieces:      1,
		StartedAt:       started,
	}}
	srv := newTestServer(t, backend, nil)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/torrents/abc/piece-download",
		strings.NewReader(`{"ranges":[{"start":0,"end":3}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := do(t, srv, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body.String())
	}

	var body struct {
		State           string     `json:"state"`
		RequestedPieces int        `json:"requested_pieces"`
		DonePieces      int        `json:"done_pieces"`
		StartedAt       time.Time  `json:"started_at"`
		FinishedAt      *time.Time `json:"finished_at"`
		Error           string     `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.State != "running" || body.RequestedPieces != 3 || body.DonePieces != 1 {
		t.Fatalf("response = %+v, want running/3/1", body)
	}
	if !body.StartedAt.Equal(started) {
		t.Fatalf("started_at = %v, want %v", body.StartedAt, started)
	}
	if body.FinishedAt != nil {
		t.Fatalf("finished_at = %v, want nil for a running download", body.FinishedAt)
	}

	if backend.pieceDownloadID != "abc" {
		t.Fatalf("backend saw id %q, want abc", backend.pieceDownloadID)
	}
	if len(backend.pieceDownloadRanges) != 1 || backend.pieceDownloadRanges[0] != (session.PieceRange{Start: 0, End: 3}) {
		t.Fatalf("backend saw ranges %+v, want [{0 3}]", backend.pieceDownloadRanges)
	}
}

func TestStartPieceDownloadReturnsFinishedAtWhenCompleted(t *testing.T) {
	finished := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
	backend := &fakeBackend{pieceDownloadStatus: session.PieceDownloadStatus{
		State:           session.PieceDownloadCompleted,
		RequestedPieces: 2,
		DonePieces:      2,
		FinishedAt:      finished,
	}}
	srv := newTestServer(t, backend, nil)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/torrents/abc/piece-download",
		strings.NewReader(`{"ranges":[{"start":0,"end":2}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := do(t, srv, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		FinishedAt *time.Time `json:"finished_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.FinishedAt == nil || !body.FinishedAt.Equal(finished) {
		t.Fatalf("finished_at = %v, want %v", body.FinishedAt, finished)
	}
}

func TestStartPieceDownloadRejectsMissingOrEmptyRanges(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing field", `{}`},
		{"empty list", `{"ranges":[]}`},
		{"invalid json", `{`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t, &fakeBackend{}, nil)
			req := httptest.NewRequest(http.MethodPut, "/api/v1/torrents/abc/piece-download", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			if rec := do(t, srv, req); rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestStartPieceDownloadRejectsOversizedBody(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, func(cfg *config.Config) {
		cfg.HTTP.MaxUploadBytes = 8
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/torrents/abc/piece-download",
		strings.NewReader(`{"ranges":[{"start":0,"end":`+strings.Repeat("9", 64)+`}]}`))
	req.Header.Set("Content-Type", "application/json")
	if rec := do(t, srv, req); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body %s", rec.Code, rec.Body.String())
	}
}

func TestStartPieceDownloadMapsBackendErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"unknown torrent", session.ErrUnknownTorrent, http.StatusNotFound},
		{"deleting", session.ErrDeleting, http.StatusConflict},
		{"not ready", session.ErrTorrentNotReady, http.StatusConflict},
		{"invalid range", errors.Join(session.ErrInvalidPieceDownloadRequest, errors.New("range [0,99) is out of bounds")), http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &fakeBackend{pieceDownloadErr: tt.err}
			srv := newTestServer(t, backend, nil)
			req := httptest.NewRequest(http.MethodPut, "/api/v1/torrents/abc/piece-download",
				strings.NewReader(`{"ranges":[{"start":0,"end":1}]}`))
			req.Header.Set("Content-Type", "application/json")
			if rec := do(t, srv, req); rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}

func TestGetPieceDownloadReturnsStatus(t *testing.T) {
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	backend := &fakeBackend{pieceDownloadStatusVal: session.PieceDownloadStatus{
		State:           session.PieceDownloadRunning,
		RequestedPieces: 5,
		DonePieces:      2,
		StartedAt:       started,
	}}
	srv := newTestServer(t, backend, nil)

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/torrents/abc/piece-download", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		State           string `json:"state"`
		RequestedPieces int    `json:"requested_pieces"`
		DonePieces      int    `json:"done_pieces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.State != "running" || body.RequestedPieces != 5 || body.DonePieces != 2 {
		t.Fatalf("response = %+v, want running/5/2", body)
	}
	if backend.pieceDownloadStatusID != "abc" {
		t.Fatalf("backend saw id %q, want abc", backend.pieceDownloadStatusID)
	}
}

func TestGetPieceDownloadMapsNoRecordToNotFound(t *testing.T) {
	backend := &fakeBackend{pieceDownloadStatusErr: session.ErrNoPieceDownload}
	srv := newTestServer(t, backend, nil)

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/torrents/abc/piece-download", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body.String())
	}
}

// TestGetPieceDownloadMapsDeletingToConflict covers the deletion window: the
// record is on its way out with the torrent, which is a conflict, not the
// "never started" absence.
func TestGetPieceDownloadMapsDeletingToConflict(t *testing.T) {
	backend := &fakeBackend{pieceDownloadStatusErr: session.ErrDeleting}
	srv := newTestServer(t, backend, nil)

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/torrents/abc/piece-download", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
}

func TestGetPieceDownloadMapsUnknownTorrentToNotFound(t *testing.T) {
	backend := &fakeBackend{pieceDownloadStatusErr: session.ErrUnknownTorrent}
	srv := newTestServer(t, backend, nil)

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/torrents/abc/piece-download", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body.String())
	}
}

// TestPieceDownloadRoutesWiredToRealSession proves *session.Session satisfies
// PieceDownloadBackend with the exact signatures the routes expect: a real
// session served through the real HTTP stack, not the fake. It does not wait
// for a transfer (there is no peer in this test), only for the routes to
// accept and describe the request.
func TestPieceDownloadRoutesWiredToRealSession(t *testing.T) {
	work := t.TempDir()
	torrentsDir := filepath.Join(work, "torrents")
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

	torrentBytes := buildTestTorrent(t, "payload.bin", []byte("piece download wiring payload"))
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", "payload.torrent")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(torrentBytes); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	addReq := httptest.NewRequest(http.MethodPost, "/api/v1/torrents", &buf)
	addReq.Header.Set("Content-Type", writer.FormDataContentType())
	rec := do(t, srv, addReq)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	var addedTorrent struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &addedTorrent); err != nil {
		t.Fatalf("decode add response: %v", err)
	}
	added := addedTorrent.ID

	// Before any piece download, the status route reports it is absent.
	rec = do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/torrents/"+added+"/piece-download", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status before any piece download = %d, want 404; body %s", rec.Code, rec.Body.String())
	}

	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/torrents/"+added+"/piece-download",
		strings.NewReader(`{"ranges":[{"start":0,"end":1}]}`))
	putReq.Header.Set("Content-Type", "application/json")
	rec = do(t, srv, putReq)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start status = %d, want 202; body %s", rec.Code, rec.Body.String())
	}
	var started struct {
		State           string `json:"state"`
		RequestedPieces int    `json:"requested_pieces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	if started.RequestedPieces != 1 {
		t.Fatalf("requested_pieces = %d, want 1", started.RequestedPieces)
	}
	if started.State != "running" && started.State != "completed" {
		t.Fatalf("state = %q, want running or completed", started.State)
	}

	rec = do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/torrents/"+added+"/piece-download", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status after start = %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	rec = do(t, srv, httptest.NewRequest(http.MethodDelete, "/api/v1/torrents/"+added+"/piece-download", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cancel status = %d, want 204; body %s", rec.Code, rec.Body.String())
	}
	rec = do(t, srv, httptest.NewRequest(http.MethodDelete, "/api/v1/torrents/"+added+"/piece-download", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("repeated cancel status = %d, want 204 (idempotent); body %s", rec.Code, rec.Body.String())
	}
}

func TestStartPieceDownloadDefaultsTimeoutSecondsTo600(t *testing.T) {
	backend := &fakeBackend{}
	srv := newTestServer(t, backend, nil)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/torrents/abc/piece-download",
		strings.NewReader(`{"ranges":[{"start":0,"end":1}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := do(t, srv, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body.String())
	}
	if backend.pieceDownloadTimeout != 600*time.Second {
		t.Fatalf("backend saw timeout %s, want the 600s default", backend.pieceDownloadTimeout)
	}
}

func TestStartPieceDownloadAcceptsExplicitTimeoutSeconds(t *testing.T) {
	backend := &fakeBackend{}
	srv := newTestServer(t, backend, nil)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/torrents/abc/piece-download",
		strings.NewReader(`{"ranges":[{"start":0,"end":1}],"timeout_seconds":30}`))
	req.Header.Set("Content-Type", "application/json")
	rec := do(t, srv, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body.String())
	}
	if backend.pieceDownloadTimeout != 30*time.Second {
		t.Fatalf("backend saw timeout %s, want 30s", backend.pieceDownloadTimeout)
	}
}

func TestStartPieceDownloadRejectsInvalidTimeoutSeconds(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"zero", `{"ranges":[{"start":0,"end":1}],"timeout_seconds":0}`},
		{"negative", `{"ranges":[{"start":0,"end":1}],"timeout_seconds":-1}`},
		{"above max", `{"ranges":[{"start":0,"end":1}],"timeout_seconds":3601}`},
		{"non-integer", `{"ranges":[{"start":0,"end":1}],"timeout_seconds":1.5}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t, &fakeBackend{}, nil)
			req := httptest.NewRequest(http.MethodPut, "/api/v1/torrents/abc/piece-download", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			rec := do(t, srv, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCancelPieceDownloadReturnsNoContent(t *testing.T) {
	backend := &fakeBackend{}
	srv := newTestServer(t, backend, nil)

	rec := do(t, srv, httptest.NewRequest(http.MethodDelete, "/api/v1/torrents/abc/piece-download", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body %s", rec.Code, rec.Body.String())
	}
	if backend.cancelPieceDownloadID != "abc" {
		t.Fatalf("backend saw id %q, want abc", backend.cancelPieceDownloadID)
	}
}

func TestCancelPieceDownloadMapsUnknownTorrentToNotFound(t *testing.T) {
	backend := &fakeBackend{cancelPieceDownloadErr: session.ErrUnknownTorrent}
	srv := newTestServer(t, backend, nil)

	rec := do(t, srv, httptest.NewRequest(http.MethodDelete, "/api/v1/torrents/abc/piece-download", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body.String())
	}
}

func TestPieceDownloadRoutesRequireAuth(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, dynamicAuth)

	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/torrents/abc/piece-download",
		strings.NewReader(`{"ranges":[{"start":0,"end":1}]}`))
	putReq.Header.Set("Content-Type", "application/json")
	if rec := do(t, srv, putReq); rec.Code != http.StatusUnauthorized {
		t.Fatalf("PUT without token status = %d, want 401; body %s", rec.Code, rec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/torrents/abc/piece-download", nil)
	if rec := do(t, srv, getReq); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET without token status = %d, want 401; body %s", rec.Code, rec.Body.String())
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/v1/torrents/abc/piece-download", nil)
	if rec := do(t, srv, deleteReq); rec.Code != http.StatusUnauthorized {
		t.Fatalf("DELETE without token status = %d, want 401; body %s", rec.Code, rec.Body.String())
	}
}
