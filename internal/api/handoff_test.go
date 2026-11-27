package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/handoff"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// handoffBackend adds the handoff surface to the shared fake backend without
// changing the fakes used by the other route tests.
type handoffBackend struct {
	*fakeBackend

	handoffID       string
	handoffOp       *session.Operation
	handoffErr      error
	handoffTarget   session.HandoffTargetView
	handoffTestView session.HandoffTargetView
	handoffTestErr  error
	handoffTestURL  string
}

func (f *handoffBackend) HandoffTorrent(_ context.Context, id string) (*session.Operation, error) {
	f.handoffID = id
	return f.handoffOp, f.handoffErr
}

func (f *handoffBackend) HandoffTarget() session.HandoffTargetView {
	return f.handoffTarget
}

func (f *handoffBackend) TestHandoffTarget(context.Context) (session.HandoffTargetView, error) {
	return f.handoffTestView, f.handoffTestErr
}

func TestHandoffTorrentReturnsAcceptedOperation(t *testing.T) {
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	backend := &handoffBackend{
		fakeBackend: &fakeBackend{},
		handoffOp:   &session.Operation{ID: "op-1", State: session.StateRunning, CreatedAt: started},
	}
	srv := newTestServer(t, backend, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/torrents/abc/handoff", nil)
	rec := do(t, srv, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body.String())
	}
	if backend.handoffID != "abc" {
		t.Fatalf("backend saw id %q, want abc", backend.handoffID)
	}
	var body struct {
		ID    string `json:"operation_id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.ID != "op-1" || body.State != "running" {
		t.Fatalf("response = %+v, want op-1/running", body)
	}
}

func TestHandoffTorrentMapsSessionErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"unconfigured", session.ErrHandoffUnconfigured, http.StatusConflict},
		{"in progress", session.ErrHandoffInProgress, http.StatusConflict},
		{"not ready", session.ErrTorrentNotReady, http.StatusConflict},
		{"unreachable", errors.Join(errors.New("dial"), handoff.ErrUnreachable), http.StatusBadGateway},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			backend := &handoffBackend{fakeBackend: &fakeBackend{}, handoffErr: testCase.err}
			srv := newTestServer(t, backend, nil)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/torrents/abc/handoff", nil)
			rec := do(t, srv, req)
			if rec.Code != testCase.code {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, testCase.code, rec.Body.String())
			}
		})
	}
}

func TestHandoffTargetReportsConfiguration(t *testing.T) {
	backend := &handoffBackend{
		fakeBackend:   &fakeBackend{},
		handoffTarget: session.HandoffTargetView{Configured: true, Target: "http://host:9091"},
	}
	srv := newTestServer(t, backend, nil)

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/handoff", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Configured bool   `json:"configured"`
		Target     string `json:"target"`
		Version    string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !body.Configured || body.Target != "http://host:9091" || body.Version != "" {
		t.Fatalf("response = %+v, want configured target without version", body)
	}
}

func TestTestHandoffTargetReportsVersion(t *testing.T) {
	backend := &handoffBackend{
		fakeBackend:     &fakeBackend{},
		handoffTestView: session.HandoffTargetView{Configured: true, Target: "http://host:9091", Version: "3.00"},
	}
	srv := newTestServer(t, backend, nil)

	rec := do(t, srv, httptest.NewRequest(http.MethodPost, "/api/v1/handoff/test", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Version != "3.00" {
		t.Fatalf("version = %q, want 3.00", body.Version)
	}
}

func TestHandoffUnavailableWithoutBackend(t *testing.T) {
	srv := newTestServer(t, &fakeBackend{}, nil)
	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/handoff", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

var _ api.Backend = (*handoffBackend)(nil)
