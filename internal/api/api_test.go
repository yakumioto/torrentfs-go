package api_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

type fakeBackend struct {
	mu sync.Mutex

	addSrc  session.Source
	addView *session.TorrentView
	addErr  error

	views      []session.TorrentView
	detailView session.TorrentView
	viewErr    error

	statusView session.TorrentStatusView
	statusErr  error

	deleteOp  *session.Operation
	deleteErr error

	ops map[string]session.Operation
}

func (f *fakeBackend) AddTorrentAndPersist(_ context.Context, src session.Source) (*session.TorrentView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addSrc = src
	if f.addErr != nil {
		return nil, f.addErr
	}
	return f.addView, nil
}

func (f *fakeBackend) ListTorrents() []session.TorrentView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.views
}

func (f *fakeBackend) TorrentViewFor(string) (session.TorrentView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.detailView, f.viewErr
}

func (f *fakeBackend) TorrentStatusFor(string) (session.TorrentStatusView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statusView, f.statusErr
}

func (f *fakeBackend) DeleteTorrent(_ context.Context, _ string, _ bool) (*session.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return f.deleteOp, nil
}

func (f *fakeBackend) Operation(id string) (session.Operation, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	op, ok := f.ops[id]
	return op, ok
}

func newTestServer(t *testing.T, backend api.Backend, tune func(*config.Config)) *api.Server {
	t.Helper()
	cfg := config.Default()
	if tune != nil {
		tune(&cfg)
	}
	srv, err := api.New(cfg, backend)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return srv
}

func do(t *testing.T, srv *api.Server, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAddMagnetJSON(t *testing.T) {
	view := session.TorrentView{
		ID:        "abc",
		InfoHash:  "abc",
		Name:      "magnet task",
		State:     session.StateAdding,
		CreatedAt: time.Now().UTC(),
	}
	backend := &fakeBackend{addView: &view}
	srv := newTestServer(t, backend, nil)

	body := `{"magnet_uri":"magnet:?xt=urn:btih:` + strings.Repeat("a", 40) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/torrents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := do(t, srv, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	if backend.addSrc.MagnetURI == "" || backend.addSrc.Metainfo != nil {
		t.Fatalf("backend source = %+v, want a magnet source", backend.addSrc)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["id"] != "abc" || got["info_hash"] != "abc" || got["state"] != "adding" {
		t.Fatalf("response = %v, want id/info_hash abc and state adding", got)
	}
}

func TestAddTorrentMultipart(t *testing.T) {
	view := session.TorrentView{ID: "abc", InfoHash: "abc", State: session.StateDownloading}
	backend := &fakeBackend{addView: &view}
	srv := newTestServer(t, backend, nil)

	payload := []byte("pretend torrent bytes")
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", "sample.torrent")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(payload); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/torrents", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := do(t, srv, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(backend.addSrc.Metainfo, payload) {
		t.Fatalf("backend metainfo = %q, want %q", backend.addSrc.Metainfo, payload)
	}
}

func TestAddRejectsBadRequests(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		want        int
	}{
		{"invalid json", "application/json", "{", http.StatusBadRequest},
		{"missing magnet", "application/json", `{}`, http.StatusBadRequest},
		{"unsupported type", "text/plain", "hello", http.StatusUnsupportedMediaType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t, &fakeBackend{}, nil)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/torrents", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.contentType)
			if rec := do(t, srv, req); rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}

func TestAddConflictWhileDeleting(t *testing.T) {
	backend := &fakeBackend{addErr: session.ErrDeleting}
	srv := newTestServer(t, backend, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/torrents", strings.NewReader(`{"magnet_uri":"magnet:?xt=urn:btih:x"}`))
	req.Header.Set("Content-Type", "application/json")
	if rec := do(t, srv, req); rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
}

func TestAddRejectsOversizedUpload(t *testing.T) {
	backend := &fakeBackend{}
	srv := newTestServer(t, backend, func(cfg *config.Config) {
		cfg.HTTP.MaxUploadBytes = 8
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/torrents", strings.NewReader(`{"magnet_uri":"`+strings.Repeat("a", 64)+`"}`))
	req.Header.Set("Content-Type", "application/json")
	if rec := do(t, srv, req); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body %s", rec.Code, rec.Body.String())
	}
}

func TestListTorrentsReturnsFields(t *testing.T) {
	backend := &fakeBackend{views: []session.TorrentView{{
		ID:             "abc",
		InfoHash:       "abc",
		Name:           "task",
		State:          session.StateSeeding,
		TotalBytes:     100,
		CompletedBytes: 100,
		Progress:       1,
		CreatedAt:      time.Now().UTC(),
		Error:          "",
	}}}
	srv := newTestServer(t, backend, nil)
	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/torrents", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("list length = %d, want 1", len(got))
	}
	for _, key := range []string{"id", "info_hash", "name", "state", "total_bytes", "completed_bytes", "progress", "created_at"} {
		if _, ok := got[0][key]; !ok {
			t.Fatalf("list entry missing %q: %v", key, got[0])
		}
	}
}

func TestTorrentDetailAndStatus(t *testing.T) {
	available := int64(42)
	view := session.TorrentView{
		ID:             strings.Repeat("a", 40),
		InfoHash:       strings.Repeat("a", 40),
		Name:           "payload",
		State:          session.StateDownloading,
		TotalBytes:     100,
		CompletedBytes: 42,
		Progress:       0.42,
		CreatedAt:      time.Now().UTC(),
	}
	backend := &fakeBackend{
		detailView: view,
		statusView: session.TorrentStatusView{
			Torrent:       view,
			MetainfoReady: true,
			PieceLength:   64,
			Pieces: []session.PieceStatus{{
				Index:          0,
				Known:          true,
				Partial:        true,
				Wanted:         true,
				AvailableBytes: &available,
			}},
			Files: []session.FileStatus{{Path: "payload", Size: 100, PieceStart: 0, PieceEnd: 2}},
		},
	}
	srv := newTestServer(t, backend, nil)

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/torrents/"+view.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var detail map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail["id"] != view.ID || detail["state"] != string(session.StateDownloading) {
		t.Fatalf("detail = %v, want torrent response", detail)
	}

	rec = do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/torrents/"+view.ID+"/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var got struct {
		MetainfoReady bool  `json:"metainfo_ready"`
		PieceLength   int64 `json:"piece_length"`
		Pieces        []struct {
			Index          int    `json:"index"`
			AvailableBytes *int64 `json:"available_bytes"`
		} `json:"pieces"`
		Files []struct {
			Path       string `json:"path"`
			PieceStart int    `json:"piece_start"`
			PieceEnd   int    `json:"piece_end"`
		} `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !got.MetainfoReady || got.PieceLength != 64 || len(got.Pieces) != 1 || got.Pieces[0].Index != 0 || got.Pieces[0].AvailableBytes == nil || *got.Pieces[0].AvailableBytes != available {
		t.Fatalf("status pieces = %+v, want one partial piece", got.Pieces)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "payload" || got.Files[0].PieceStart != 0 || got.Files[0].PieceEnd != 2 {
		t.Fatalf("status files = %+v, want one piece range", got.Files)
	}
}

func TestTorrentStatusPendingAndUnknown(t *testing.T) {
	id := strings.Repeat("b", 40)
	view := session.TorrentView{ID: id, InfoHash: id, State: session.StateAdding}
	backend := &fakeBackend{statusView: session.TorrentStatusView{
		Torrent: view,
		Pieces:  []session.PieceStatus{},
		Files:   []session.FileStatus{},
	}}
	srv := newTestServer(t, backend, nil)
	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/torrents/"+id+"/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pending status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var pending map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &pending); err != nil {
		t.Fatalf("decode pending status: %v", err)
	}
	if pending["metainfo_ready"] != false {
		t.Fatalf("pending metainfo_ready = %v, want false", pending["metainfo_ready"])
	}
	if pieces, ok := pending["pieces"].([]any); !ok || len(pieces) != 0 {
		t.Fatalf("pending pieces = %v, want []", pending["pieces"])
	}

	backend.statusErr = session.ErrUnknownTorrent
	rec = do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/torrents/missing/status", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown status = %d, want 404; body %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteReturnsAcceptedOperation(t *testing.T) {
	backend := &fakeBackend{deleteOp: &session.Operation{
		ID:        "op-1",
		TorrentID: "abc",
		State:     session.StateDeleting,
	}}
	srv := newTestServer(t, backend, nil)
	rec := do(t, srv, httptest.NewRequest(http.MethodDelete, "/api/v1/torrents/abc?purge_data=true", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["operation_id"] != "op-1" || got["state"] != "deleting" {
		t.Fatalf("response = %v, want operation_id op-1 and state deleting", got)
	}
}

func TestDeleteRejectsInvalidPurgeFlag(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, nil)
	rec := do(t, srv, httptest.NewRequest(http.MethodDelete, "/api/v1/torrents/abc?purge_data=maybe", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDeleteConflictOnExternalReference(t *testing.T) {
	backend := &fakeBackend{deleteErr: session.ErrExternalReference}
	srv := newTestServer(t, backend, nil)
	rec := do(t, srv, httptest.NewRequest(http.MethodDelete, "/api/v1/torrents/abc", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestDeleteUnknownTorrent(t *testing.T) {
	backend := &fakeBackend{deleteErr: session.ErrUnknownTorrent}
	srv := newTestServer(t, backend, nil)
	rec := do(t, srv, httptest.NewRequest(http.MethodDelete, "/api/v1/torrents/does-not-exist", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestOperationLookup(t *testing.T) {
	backend := &fakeBackend{ops: map[string]session.Operation{
		"op-1": {ID: "op-1", TorrentID: "abc", State: session.StateDeleted},
	}}
	srv := newTestServer(t, backend, nil)

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/operations/op-1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["operation_id"] != "op-1" || got["state"] != "deleted" {
		t.Fatalf("operation = %v, want op-1/deleted", got)
	}

	rec = do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/operations/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing operation status = %d, want 404", rec.Code)
	}
}

func TestServerAgainstRealSession(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := filepath.Join(work, "torrents")
	if err := os.MkdirAll(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	cfg := config.Default()
	cfg.Paths.DataDir = dataDir

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

	torrentBytes := buildTestTorrent(t, "payload.bin", []byte("api end to end payload"))
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
	req := httptest.NewRequest(http.MethodPost, "/api/v1/torrents", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := do(t, srv, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
	var added struct {
		ID       string `json:"id"`
		InfoHash string `json:"info_hash"`
		Name     string `json:"name"`
		State    string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &added); err != nil {
		t.Fatalf("decode add: %v", err)
	}
	if added.Name != "payload.bin" || added.InfoHash == "" {
		t.Fatalf("add response = %+v, want name and info hash", added)
	}

	rec = do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/torrents", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", rec.Code)
	}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0]["id"] != added.ID {
		t.Fatalf("list = %v, want the added torrent", list)
	}

	rec = do(t, srv, httptest.NewRequest(http.MethodDelete, "/api/v1/torrents/"+added.ID, nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("delete status = %d, want 202; body %s", rec.Code, rec.Body.String())
	}
	var op struct {
		OperationID string `json:"operation_id"`
		State       string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &op); err != nil {
		t.Fatalf("decode delete: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for op.State == string(session.StateDeleting) {
		if time.Now().After(deadline) {
			t.Fatal("delete operation never finished")
		}
		time.Sleep(10 * time.Millisecond)
		rec = do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/operations/"+op.OperationID, nil))
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

	rec = do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/torrents", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("final list status = %d, want 200", err)
	}
	list = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode final list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("final list = %v, want empty", list)
	}
}

func buildTestTorrent(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	const pieceLength = 256 << 10
	sum := sha1.Sum(data)
	info := metainfo.Info{
		Name:        name,
		Length:      int64(len(data)),
		PieceLength: pieceLength,
		Pieces:      sum[:],
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatalf("encode info: %v", err)
	}
	mi := metainfo.MetaInfo{InfoBytes: bencode.Bytes(infoBytes)}
	out, err := bencode.Marshal(mi)
	if err != nil {
		t.Fatalf("encode metainfo: %v", err)
	}
	return out
}
