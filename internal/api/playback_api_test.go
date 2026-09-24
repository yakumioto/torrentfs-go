package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

type playbackBackendFake struct {
	*fakeBackend
	stream  session.PlaybackStreamSnapshot
	stopped string
}

func (f *playbackBackendFake) StartPlaybackStream(_ context.Context, torrentID string, request session.PlaybackStreamStart) (session.PlaybackStreamSnapshot, error) {
	f.stream = session.PlaybackStreamSnapshot{ID: "stream-1", TorrentID: torrentID, Path: request.Path, PlaybackCursor: request.PositionBytes}
	return f.stream, nil
}

func (f *playbackBackendFake) UpdatePlaybackStream(_ context.Context, id string, request session.PlaybackStreamUpdate) (session.PlaybackStreamSnapshot, error) {
	if id != f.stream.ID {
		return session.PlaybackStreamSnapshot{}, session.ErrPlaybackStreamNotFound
	}
	f.stream.LastSequence = request.Sequence
	f.stream.PlaybackCursor = request.PositionBytes
	return f.stream, nil
}

func (f *playbackBackendFake) StopPlaybackStream(_ context.Context, id string) error {
	if id != f.stream.ID {
		return session.ErrPlaybackStreamNotFound
	}
	f.stopped = id
	return nil
}

func TestPlaybackLifecycleRoutes(t *testing.T) {
	backend := &playbackBackendFake{fakeBackend: &fakeBackend{}}
	srv := newTestServer(t, backend, nil)

	start := httptest.NewRequest(http.MethodPost, "/api/v1/torrents/torrent-1/playback-streams", strings.NewReader(`{"path":"payload.bin","position_bytes":128}`))
	start.Header.Set("Content-Type", "application/json")
	rec := do(t, srv, start)
	if rec.Code != http.StatusCreated {
		t.Fatalf("start status = %d; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"stream_id":"stream-1"`) {
		t.Fatalf("start response = %s", rec.Body.String())
	}

	update := httptest.NewRequest(http.MethodPatch, "/api/v1/playback-streams/stream-1", strings.NewReader(`{"sequence":1,"event":"progress","position_bytes":192}`))
	update.Header.Set("Content-Type", "application/json")
	rec = do(t, srv, update)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"playback_cursor":192`) {
		t.Fatalf("update response = %d %s", rec.Code, rec.Body.String())
	}

	stop := httptest.NewRequest(http.MethodDelete, "/api/v1/playback-streams/stream-1", nil)
	rec = do(t, srv, stop)
	if rec.Code != http.StatusNoContent || backend.stopped != "stream-1" {
		t.Fatalf("stop response = %d, stopped=%q", rec.Code, backend.stopped)
	}
}
