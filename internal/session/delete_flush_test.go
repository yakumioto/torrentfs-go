package session_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

// newDeleteFlushSession builds a loopback-only session so deletion can talk to
// a local tracker without touching the network.
func newDeleteFlushSession(t *testing.T, torrentsDir string, opts ...session.Option) *session.Session {
	t.Helper()
	if err := os.MkdirAll(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	cfg := testConfig()
	cfg.Connections.ListenHost = "127.0.0.1"
	sess, err := session.NewWithClientConfig(cfg, torrentsDir, func(cc *session.TorrentClientConfig) {
		cc.NoDHT = true
		cc.DisableUTP = true
		cc.DisableIPv6 = true
		cc.NoDefaultPortForwarding = true
	}, opts...)
	if err != nil {
		t.Fatalf("NewWithClientConfig: %v", err)
	}
	t.Cleanup(func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return sess
}

func addTrackedTorrent(t *testing.T, ctx context.Context, sess *session.Session, tracker *loopbackTracker, name string) string {
	t.Helper()
	content := make([]byte, 16<<10)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, name, content, [][]string{{tracker.url}})
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("AddTorrentAndPersist: %v", err)
	}
	hashHex := hash.HexString()
	if err := waitFor(ctx, func() bool { return tracker.announceCount(hashHex) >= 1 }); err != nil {
		t.Fatalf("tracker did not see the started announce: %v", err)
	}
	return hashHex
}

func countStoppedAnnounces(tracker *loopbackTracker, hashHex string) int {
	count := 0
	for _, announce := range tracker.announceSnapshot(hashHex) {
		if announce.Event == "stopped" {
			count++
		}
	}
	return count
}

// Deleting a torrent must tell the tracker that it stopped while the torrent is
// still registered, instead of leaving the final announce to a dispatcher that
// only runs after the torrent is gone.
func TestDeleteFlushesStoppedAnnounce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	work := t.TempDir()
	tracker := newLoopbackTracker(t)
	sess := newDeleteFlushSession(t, filepath.Join(work, "torrents"))
	hashHex := addTrackedTorrent(t, ctx, sess, tracker, "payload.bin")

	op, err := sess.DeleteTorrent(ctx, hashHex)
	if err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}
	final := waitOperation(t, sess, op.ID)
	if final.State != session.StateDeleted {
		t.Fatalf("delete state = %s (%s), want deleted", final.State, final.Error)
	}
	if final.Warning != "" {
		t.Fatalf("delete warning = %q, want none", final.Warning)
	}
	if got := countStoppedAnnounces(tracker, hashHex); got < 1 {
		t.Fatalf("tracker saw %d stopped announces, want at least 1", got)
	}
}

// Exercise actual peer payload, not just a zero-byte stopped event. The final
// announce must include bytes transferred since the previous regular announce.
func TestDeleteFlushesFinalRealUpload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tracker := newLoopbackTracker(t)
	work := t.TempDir()
	seeder := newDeleteFlushSession(t, filepath.Join(work, "seeder"))
	leecher := newDeleteFlushSession(t, filepath.Join(work, "leecher"))
	content := bytes.Repeat([]byte{0x5a}, 16<<10)
	meta, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, [][]string{{tracker.url}})
	hashHex := hash.HexString()
	if _, err := seeder.AddTorrentAndPersist(ctx, session.Source{Metainfo: meta}); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(ctx, func() bool { return tracker.announceCount(hashHex) >= 1 }); err != nil {
		t.Fatal(err)
	}
	seederID := tracker.announceSnapshot(hashHex)[0].PeerID
	seedPieces(t, seeder, hash, content)
	if err := leecher.AddTorrent(ctx, session.Source{Metainfo: meta}); err != nil {
		t.Fatal(err)
	}
	reader, err := leecher.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(content))
	if _, err := io.ReadFull(io.NewSectionReader(reader, 0, int64(len(got))), got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("peer payload mismatch")
	}
	realUploaded := seeder.RuntimeStats().UploadedBytes
	if realUploaded != int64(len(content)) {
		t.Fatalf("real uploaded = %d, want %d", realUploaded, len(content))
	}
	op, err := seeder.DeleteTorrent(ctx, hashHex)
	if err != nil {
		t.Fatal(err)
	}
	final := waitOperation(t, seeder, op.ID)
	if final.State != session.StateDeleted || final.Warning != "" {
		t.Fatalf("delete = %+v", final)
	}
	for _, announce := range tracker.announceSnapshot(hashHex) {
		if announce.Event == "stopped" && bytes.Equal(announce.PeerID, seederID) {
			if announce.Uploaded != realUploaded {
				t.Fatalf("stopped uploaded = %d, real uploaded = %d", announce.Uploaded, realUploaded)
			}
			t.Logf("real uploaded = %d; tracker stopped uploaded = %d", realUploaded, announce.Uploaded)
			return
		}
	}
	t.Fatal("tracker never received the seeder's final upload")
}

// A tracker that rejects the stopped announce must not turn a completed
// deletion into a failure. The loss is reported as a warning on the operation.
func TestDeleteRecordsStoppedFlushFailureAsWarning(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	work := t.TempDir()
	tracker := newLoopbackTracker(t)
	sess := newDeleteFlushSession(t, filepath.Join(work, "torrents"))
	hashHex := addTrackedTorrent(t, ctx, sess, tracker, "payload.bin")
	tracker.failStopped(http.StatusInternalServerError)

	op, err := sess.DeleteTorrent(ctx, hashHex)
	if err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}
	final := waitOperation(t, sess, op.ID)
	if final.State != session.StateDeleted {
		t.Fatalf("delete state = %s (%s), want deleted", final.State, final.Error)
	}
	if final.Error != "" {
		t.Fatalf("delete error = %q, want empty", final.Error)
	}
	if final.Warning == "" {
		t.Fatal("delete warning is empty, want the failed stopped announce")
	}
	if !strings.Contains(final.Warning, "stopped") {
		t.Fatalf("delete warning = %q, want it to name the stopped announce", final.Warning)
	}
	if views := sess.ListTorrents(); len(views) != 0 {
		t.Fatalf("ListTorrents after delete = %+v, want empty", views)
	}
}

// The synchronous flush is bounded: a tracker that never answers must not keep
// the deletion operation running.
func TestDeleteStoppedFlushTimeoutIsBounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	work := t.TempDir()
	tracker := newLoopbackTracker(t)
	sess := newDeleteFlushSession(t, filepath.Join(work, "torrents"), session.WithDeleteFlushTimeoutForTest(200*time.Millisecond))
	hashHex := addTrackedTorrent(t, ctx, sess, tracker, "payload.bin")
	tracker.blockStopped()

	start := time.Now()
	op, err := sess.DeleteTorrent(ctx, hashHex)
	if err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}
	final := waitOperation(t, sess, op.ID)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("delete took %v, want the flush bounded well below the tracker timeout", elapsed)
	}
	if final.State != session.StateDeleted {
		t.Fatalf("delete state = %s (%s), want deleted", final.State, final.Error)
	}
	if final.Warning == "" {
		t.Fatal("delete warning is empty, want the flush timeout")
	}
}
