package session_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/experiments"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// TestStartupReportedUploadSurvivesDeletionThroughHTTP exercises the HTTP contract
// with a real session and loopback tracker, including the stopped announce path.
func TestStartupReportedUploadSurvivesDeletionThroughHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tracker := newLoopbackTracker(t)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", []byte("startup reported upload"), [][]string{{tracker.url}})
	torrentsDir := filepath.Join(t.TempDir(), "torrents")
	settings := experiments.Settings{RuntimeSettings: experiments.RuntimeSettings{
		Mode: experiments.ModeInject, RateBytesPerSecond: 1 << 20, CapBytes: 64,
	}}
	sess := newExperimentSession(t, torrentsDir, settings)

	assertHTTP := func(sess *session.Session, wantStartup, wantCurrent int64, wantTorrents int) {
		t.Helper()
		srv, err := api.New(config.Default(), sess)
		if err != nil {
			t.Fatalf("api.New: %v", err)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/experiments/upload", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET experiment = %d: %s", rec.Code, rec.Body.String())
		}
		var got struct {
			Startup *int64 `json:"startup_reported_uploaded_bytes"`
			Totals  struct {
				Reported int64 `json:"reported_uploaded_bytes"`
			} `json:"totals"`
			Torrents []json.RawMessage `json:"torrents"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode experiment: %v", err)
		}
		if got.Startup == nil || *got.Startup != wantStartup || got.Totals.Reported != wantCurrent || len(got.Torrents) != wantTorrents {
			t.Fatalf("experiment = %s, want startup=%d current=%d torrents=%d", rec.Body.String(), wantStartup, wantCurrent, wantTorrents)
		}
	}

	assertHTTP(sess, 0, 0, 0)
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("AddTorrentAndPersist: %v", err)
	}
	if err := waitFor(ctx, func() bool { return tracker.announceCount(hash.HexString()) >= 3 }); err != nil {
		t.Fatalf("wait for repeated announces: %v", err)
	}
	assertHTTP(sess, 64, 64, 1)

	op, err := sess.DeleteTorrent(ctx, hash.HexString())
	if err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}
	if final := waitOperation(t, sess, op.ID); final.State != session.StateDeleted {
		t.Fatalf("delete state = %s (%s), want deleted", final.State, final.Error)
	}
	assertHTTP(sess, 64, 0, 0)

	if err := sess.Close(ctx); err != nil {
		t.Fatalf("Close before restart: %v", err)
	}
	restarted := newExperimentSession(t, torrentsDir, settings)
	assertHTTP(restarted, 0, 0, 0)
}
