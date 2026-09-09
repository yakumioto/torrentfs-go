package session_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
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
	if !st.Seeding() {
		t.Fatal("complete torrent is not seeding")
	}
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

func commitMetadata(t *testing.T, sess *session.Session, name string, data []byte) {
	t.Helper()
	w, err := sess.BeginMetadata(context.Background(), name, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL)
	if err != nil {
		t.Fatalf("BeginMetadata(%q): %v", name, err)
	}
	if _, err := w.WriteAt(data, 0); err != nil {
		t.Fatalf("WriteAt(%q): %v", name, err)
	}
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit(%q): %v", name, err)
	}
}

func TestSessionMetadataLifecycle(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", []byte("metadata lifecycle"))
	torrentBytes, err := os.ReadFile(torrentPath)
	if err != nil {
		t.Fatalf("read torrent: %v", err)
	}

	sess, err := session.New(config.Config{Paths: config.Paths{DataDir: dataDir}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	commitMetadata(t, sess, "payload.x0.torrent", torrentBytes)
	files := sess.MetadataFiles()
	if len(files) != 1 || files[0].Name != "payload.x0.torrent" || files[0].Size != int64(len(torrentBytes)) {
		t.Fatalf("MetadataFiles = %+v", files)
	}
	if _, ok := sess.Torrent(hash); !ok {
		t.Fatal("committed metadata did not register torrent")
	}

	if err := sess.RenameMetadata(context.Background(), "payload.x0.torrent", "renamed.torrent"); err != nil {
		t.Fatalf("RenameMetadata: %v", err)
	}
	if _, err := sess.OpenMetadata("renamed.torrent"); err != nil {
		t.Fatalf("OpenMetadata after rename: %v", err)
	}
	if err := sess.RemoveMetadata(context.Background(), "renamed.torrent"); err != nil {
		t.Fatalf("RemoveMetadata: %v", err)
	}
	if len(sess.List()) != 0 {
		t.Fatalf("List after remove = %d, want 0", len(sess.List()))
	}
	if err := sess.RemoveMetadataDir(); err != nil {
		t.Fatalf("RemoveMetadataDir: %v", err)
	}
	if err := sess.EnsureMetadataDir(); err != nil {
		t.Fatalf("EnsureMetadataDir: %v", err)
	}
}

func TestSessionMetadataDuplicateReferences(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentPath, _ := buildSingleFileTorrent(t, dataDir, work, "payload.bin", []byte("duplicate metadata"))
	torrentBytes, err := os.ReadFile(torrentPath)
	if err != nil {
		t.Fatalf("read torrent: %v", err)
	}
	sess, err := session.New(config.Config{Paths: config.Paths{DataDir: dataDir}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sess.Close(context.Background())

	commitMetadata(t, sess, "one.torrent", torrentBytes)
	commitMetadata(t, sess, "two.torrent", torrentBytes)
	if len(sess.List()) != 1 {
		t.Fatalf("List after duplicate metadata = %d, want 1", len(sess.List()))
	}
	if err := sess.RemoveMetadata(context.Background(), "one.torrent"); err != nil {
		t.Fatalf("remove first metadata: %v", err)
	}
	if len(sess.List()) != 1 {
		t.Fatalf("List after first remove = %d, want 1", len(sess.List()))
	}
	if err := sess.RemoveMetadata(context.Background(), "two.torrent"); err != nil {
		t.Fatalf("remove second metadata: %v", err)
	}
	if len(sess.List()) != 0 {
		t.Fatalf("List after second remove = %d, want 0", len(sess.List()))
	}
}

func TestSessionMetadataCommitFailureCleansTemporaryFile(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	sess, err := session.New(config.Config{Paths: config.Paths{DataDir: dataDir}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sess.Close(context.Background())

	w, err := sess.BeginMetadata(context.Background(), "invalid.torrent", syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL)
	if err != nil {
		t.Fatalf("BeginMetadata: %v", err)
	}
	if _, err := w.WriteAt([]byte("not a torrent"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := w.Commit(); err == nil {
		t.Fatal("Commit invalid metainfo succeeded")
	}
	if got := sess.MetadataFiles(); len(got) != 0 {
		t.Fatalf("MetadataFiles after failed commit = %+v", got)
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "metadata"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files remain after failed commit: %+v", entries)
	}
}

func TestSessionCloseIsIdempotentAndRejectsNewOperations(t *testing.T) {
	work := t.TempDir()
	sess, err := session.New(config.Config{Paths: config.Paths{DataDir: filepath.Join(work, "data")}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := sess.AddTorrent(context.Background(), session.Source{MetainfoPath: "missing.torrent"}); !errors.Is(err, filesystem.ErrClosed) {
		t.Fatalf("AddTorrent after Close = %v, want ErrClosed", err)
	}
}
