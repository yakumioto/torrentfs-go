package session_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

func waitComplete(t *testing.T, ctx context.Context, st *session.Torrent) {
	t.Helper()
	select {
	case <-st.GotInfo():
	case <-ctx.Done():
		t.Fatalf("timed out waiting for torrent info")
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if st.BytesCompleted() == st.Length() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("torrent never became complete: %d/%d bytes", st.BytesCompleted(), st.Length())
}

// TestSessionReadsExistingData exercises the offline happy path: add a local
// single-file torrent whose data already exists under the data dir, wait for
// it to be verified, then read it back through the Backend view.
func TestSessionReadsExistingData(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	work := t.TempDir()
	dataDir := work + "/data"
	content := []byte(strings.Repeat("hello torrentfs\n", 200)) // ~3.4 KiB
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", content)

	cfg := config.Config{Paths: config.Paths{DataDir: dataDir}}
	sess, err := session.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered by hash")
	}
	waitComplete(t, ctx, st)
	if got := st.Name(); got != "payload.bin" {
		t.Fatalf("Name = %q, want payload.bin", got)
	}

	views := sess.Torrents()
	if len(views) != 1 {
		t.Fatalf("Torrents() = %d views, want 1", len(views))
	}
	v := views[0]
	if v.Hash != hash || v.Name != "payload.bin" {
		t.Fatalf("view = %+v, want torrent %s named payload.bin", v, hash)
	}
	if len(v.Files) != 1 || v.Files[0].Path != "payload.bin" || v.Files[0].Size != int64(len(content)) {
		t.Fatalf("view files = %+v, want single payload.bin of %d bytes", v.Files, len(content))
	}

	ra, err := sess.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	got := make([]byte, len(content))
	if _, err := io.ReadFull(io.NewSectionReader(ra, 0, int64(len(content))), got); err != nil {
		t.Fatalf("read full file: %v", err)
	}
	if string(got) != string(content) {
		t.Fatal("read content differs from source")
	}

	// ReadAt from the middle: verifies seek+read on the shared handle.
	buf := make([]byte, 5)
	if _, err := ra.ReadAt(buf, 6); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buf) != string(content[6:11]) {
		t.Fatalf("ReadAt(6) = %q, want %q", buf, content[6:11])
	}
}

// TestSessionDuplicateAddIsIdempotent registers the same torrent twice.
func TestSessionDuplicateAddIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	work := t.TempDir()
	dataDir := work + "/data"
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", []byte("dup me"))

	sess, err := session.New(config.Config{Paths: config.Paths{DataDir: dataDir}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("second AddTorrent: %v", err)
	}
	if n := len(sess.List()); n != 1 {
		t.Fatalf("List() = %d torrents, want 1", n)
	}
	if _, ok := sess.Torrent(hash); !ok {
		t.Fatal("torrent missing after duplicate add")
	}
}
