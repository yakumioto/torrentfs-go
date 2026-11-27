package session_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// handoffServer is a minimal Transmission RPC endpoint that records the
// metainfo of every torrent-add it receives.
type handoffServer struct {
	metainfo chan []byte
	fail     bool
}

func newHandoffServer(t *testing.T, fail bool) (*handoffServer, *httptest.Server) {
	t.Helper()
	fake := &handoffServer{metainfo: make(chan []byte, 1), fail: fail}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fake.fail {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		var request struct {
			Method    string `json:"method"`
			Arguments struct {
				Metainfo string `json:"metainfo"`
			} `json:"arguments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Method != "torrent-add" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		data, err := base64.StdEncoding.DecodeString(request.Arguments.Metainfo)
		if err != nil {
			http.Error(w, "bad metainfo", http.StatusBadRequest)
			return
		}
		select {
		case fake.metainfo <- data:
		default:
		}
		writeJSON(t, w, map[string]any{
			"result":    "success",
			"arguments": map[string]any{"torrent-added": map[string]any{"id": 1, "name": "payload", "hashString": "00"}},
		})
	}))
	t.Cleanup(server.Close)
	return fake, server
}

func writeJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func configureHandoffTarget(t *testing.T, sess *session.Session, url string) {
	t.Helper()
	err := sess.ApplyHandoffConfig(config.Config{Handoff: config.Handoff{Transmission: config.HandoffTransmission{URL: url}}})
	if err != nil {
		t.Fatalf("ApplyHandoffConfig: %v", err)
	}
}

// waitHandoffOperation polls until the handoff operation reaches a terminal
// state.
func waitHandoffOperation(t *testing.T, sess *session.Session, id string) session.Operation {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		op, ok := sess.Operation(id)
		if ok && op.State != session.StateRunning && op.State != session.StateDeleting {
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("handoff operation %s never reached a terminal state", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func metainfoPath(torrentsDir string, hash metainfo.Hash) string {
	return filepath.Join(torrentsDir, hash.HexString()+".torrent")
}

func TestHandoffDeliversMetainfoAndDeletesLocally(t *testing.T) {
	torrentsDir := testTorrentDir(t, filepath.Join(t.TempDir(), "data"))
	sess := newManageSession(t, torrentsDir)
	data, hash := multiFileTorrentBytes(t, "Payload", map[string][]byte{"payload.bin": []byte("content")})
	addTorrentBytes(t, sess, data)

	fake, server := newHandoffServer(t, false)
	configureHandoffTarget(t, sess, server.URL)

	op, err := sess.HandoffTorrent(context.Background(), hash.HexString())
	if err != nil {
		t.Fatalf("HandoffTorrent: %v", err)
	}
	if op.State != session.StateRunning {
		t.Fatalf("operation state = %q, want running", op.State)
	}
	done := waitHandoffOperation(t, sess, op.ID)
	if done.State != session.StateCompleted {
		t.Fatalf("operation state = %q (%s), want completed", done.State, done.Error)
	}

	select {
	case delivered := <-fake.metainfo:
		reader, err := metainfo.Load(bytes.NewReader(delivered))
		if err != nil {
			t.Fatalf("load delivered metainfo: %v", err)
		}
		if got := reader.HashInfoBytes(); got != hash {
			t.Fatalf("delivered infohash = %s, want %s", got.HexString(), hash.HexString())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the target never received the metainfo")
	}

	if _, err := sess.TorrentStatusFor(hash.HexString()); err == nil {
		t.Fatal("the torrent is still managed after a completed handoff")
	}
	if _, err := os.Stat(metainfoPath(torrentsDir, hash)); !os.IsNotExist(err) {
		t.Fatalf("metainfo file still exists: %v", err)
	}
}

func TestHandoffRestoresTorrentWhenDeliveryFails(t *testing.T) {
	torrentsDir := testTorrentDir(t, filepath.Join(t.TempDir(), "data"))
	sess := newManageSession(t, torrentsDir)
	data, hash := multiFileTorrentBytes(t, "Payload", map[string][]byte{"payload.bin": []byte("content")})
	addTorrentBytes(t, sess, data)

	_, server := newHandoffServer(t, true)
	configureHandoffTarget(t, sess, server.URL)

	op, err := sess.HandoffTorrent(context.Background(), hash.HexString())
	if err != nil {
		t.Fatalf("HandoffTorrent: %v", err)
	}
	done := waitHandoffOperation(t, sess, op.ID)
	if done.State != session.StateRestored {
		t.Fatalf("operation state = %q, want restored", done.State)
	}
	if done.ErrorCode != session.HandoffCodeUnreachable {
		t.Fatalf("error code = %q, want %q", done.ErrorCode, session.HandoffCodeUnreachable)
	}
	status, err := sess.TorrentStatusFor(hash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor after restore: %v", err)
	}
	if status.Torrent.State != session.StateReady {
		t.Fatalf("torrent state = %q, want ready", status.Torrent.State)
	}
	if _, err := os.Stat(metainfoPath(torrentsDir, hash)); err != nil {
		t.Fatalf("metainfo file missing after restore: %v", err)
	}
}

func TestHandoffRequiresConfiguration(t *testing.T) {
	sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))
	data, hash := multiFileTorrentBytes(t, "Payload", map[string][]byte{"payload.bin": []byte("content")})
	addTorrentBytes(t, sess, data)

	if _, err := sess.HandoffTorrent(context.Background(), hash.HexString()); err == nil {
		t.Fatal("handoff succeeded without a configured target")
	} else if !errors.Is(err, session.ErrHandoffUnconfigured) {
		t.Fatalf("error = %v, want ErrHandoffUnconfigured", err)
	}
}

func TestHandoffTargetViewReportsEndpoint(t *testing.T) {
	sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))

	if view := sess.HandoffTarget(); view.Configured || view.Target != "" {
		t.Fatalf("view = %+v, want an unconfigured target", view)
	}

	configureHandoffTarget(t, sess, "http://nas:9091/transmission/rpc")
	view := sess.HandoffTarget()
	if !view.Configured {
		t.Fatal("Configured = false, want true after configuring the target")
	}
	if view.Target != "http://nas:9091/transmission/rpc" {
		t.Fatalf("Target = %q, want the configured endpoint URL", view.Target)
	}
}
