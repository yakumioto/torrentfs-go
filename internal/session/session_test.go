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

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

type cancelAfterFirstCheckContext struct {
	errChecks int
	done      chan struct{}
}

func newCancelAfterFirstCheckContext() *cancelAfterFirstCheckContext {
	return &cancelAfterFirstCheckContext{done: make(chan struct{})}
}

func (c *cancelAfterFirstCheckContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterFirstCheckContext) Done() <-chan struct{}       { return c.done }
func (c *cancelAfterFirstCheckContext) Err() error {
	c.errChecks++
	if c.errChecks > 1 {
		select {
		case <-c.done:
		default:
			close(c.done)
		}
		return context.Canceled
	}
	return nil
}
func (c *cancelAfterFirstCheckContext) Value(any) any { return nil }

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

	cfg := testConfig(dataDir)
	sess, err := session.New(cfg, testTorrentDir(t, dataDir))
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
	if !v.SingleFile {
		t.Fatal("single-file torrent view must set SingleFile so the mount exposes it directly")
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

func TestSessionAddTorrentMissingMetainfoPreservesPathError(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	missing := filepath.Join(work, "inputs", "missing.torrent")

	sess, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	err = sess.AddTorrent(context.Background(), session.Source{MetainfoPath: missing})
	if err == nil {
		t.Fatal("AddTorrent succeeded for missing metainfo")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("AddTorrent error = %v, want os.ErrNotExist", err)
	}
	for _, want := range []string{"session: add torrent", missing, "no such file or directory"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("AddTorrent error = %q, want %q", err, want)
		}
	}
	if got := len(sess.List()); got != 0 {
		t.Fatalf("List after missing metainfo = %d, want 0", got)
	}
}

// TestSessionDuplicateAddIsIdempotent registers the same torrent twice.
func TestSessionDuplicateAddIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	work := t.TempDir()
	dataDir := work + "/data"
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", []byte("dup me"))

	sess, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
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

func TestSessionDuplicateAddCancellationPreservesExistingTorrent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	content := []byte("duplicate cancellation")
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", content)

	sess, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	source := session.Source{MetainfoPath: torrentPath}
	if err := sess.AddTorrent(ctx, source); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent missing after initial add")
	}
	waitComplete(t, ctx, st)
	if !st.Seeding() {
		t.Fatal("initial torrent is not seeding")
	}

	if err := sess.AddTorrent(newCancelAfterFirstCheckContext(), source); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled duplicate AddTorrent = %v, want context.Canceled", err)
	}
	current, ok := sess.Torrent(hash)
	if !ok || current != st {
		t.Fatalf("torrent after cancelled duplicate = (%p, %v), want original (%p, true)", current, ok, st)
	}
	if !current.Seeding() {
		t.Fatal("cancelled duplicate stopped the existing torrent")
	}

	ra, err := sess.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatalf("OpenFile after cancelled duplicate: %v", err)
	}
	got := make([]byte, len(content))
	if _, err := io.ReadFull(io.NewSectionReader(ra, 0, int64(len(content))), got); err != nil {
		t.Fatalf("read after cancelled duplicate: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("read after cancelled duplicate = %q, want %q", got, content)
	}

	if err := sess.AddTorrent(context.Background(), source); err != nil {
		t.Fatalf("subsequent duplicate AddTorrent: %v", err)
	}
	if current, ok := sess.Torrent(hash); !ok || current != st || !current.Seeding() {
		t.Fatalf("torrent after subsequent duplicate = (%p, %v), want original seeding torrent", current, ok)
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

func TestSessionMetadataRootDoesNotConflictWithTorrentNamedMetadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	content := []byte("torrent named metadata")
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "metadata", content)
	torrentBytes, err := os.ReadFile(torrentPath)
	if err != nil {
		t.Fatalf("read torrent: %v", err)
	}

	sess, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	dataPath := filepath.Join(dataDir, "metadata")
	if info, err := os.Stat(dataPath); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("torrent data path = (%v, %v), want regular file", info, err)
	}
	metadataDir := filepath.Join(testTorrentDir(t, dataDir), ".metadata")
	if info, err := os.Stat(metadataDir); err != nil || !info.IsDir() {
		t.Fatalf("metadata root = (%v, %v), want directory", info, err)
	}

	source := session.Source{MetainfoPath: torrentPath}
	if err := sess.AddTorrent(ctx, source); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent named metadata was not registered")
	}
	waitComplete(t, ctx, st)

	ra, err := sess.OpenFile(hash, "metadata")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	got := make([]byte, len(content))
	if _, err := io.ReadFull(io.NewSectionReader(ra, 0, int64(len(content))), got); err != nil {
		t.Fatalf("read torrent data: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("torrent data = %q, want %q", got, content)
	}

	commitMetadata(t, sess, "control.torrent", torrentBytes)
	if files := sess.MetadataFiles(); len(files) != 1 || files[0].Name != "control.torrent" {
		t.Fatalf("MetadataFiles = %+v, want control.torrent", files)
	}
	if _, err := os.Stat(filepath.Join(metadataDir, "control.torrent")); err != nil {
		t.Fatalf("sibling metadata file missing: %v", err)
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

	sess, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
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

func TestSessionRestoresMetadataAfterRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	content := []byte(strings.Repeat("restored metadata\n", 200))
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", content)
	torrentBytes, err := os.ReadFile(torrentPath)
	if err != nil {
		t.Fatalf("read torrent: %v", err)
	}

	first, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	commitMetadata(t, first, "restored.torrent", torrentBytes)
	st, ok := first.Torrent(hash)
	if !ok {
		t.Fatal("first session did not register metadata torrent")
	}
	waitComplete(t, ctx, st)
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer func() {
		if err := second.Close(context.Background()); err != nil {
			t.Errorf("second Close: %v", err)
		}
	}()

	files := second.MetadataFiles()
	if len(files) != 1 || files[0].Name != "restored.torrent" || files[0].Size != int64(len(torrentBytes)) {
		t.Fatalf("MetadataFiles after restart = %+v", files)
	}
	st, ok = second.Torrent(hash)
	if !ok {
		t.Fatal("restored torrent missing from session")
	}
	waitComplete(t, ctx, st)
	views := second.Torrents()
	if len(views) != 1 || views[0].Hash != hash || views[0].Name != "payload.bin" {
		t.Fatalf("Torrents after restart = %+v", views)
	}

	ra, err := second.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatalf("OpenFile after restart: %v", err)
	}
	got := make([]byte, len(content))
	if _, err := io.ReadFull(io.NewSectionReader(ra, 0, int64(len(content))), got); err != nil {
		t.Fatalf("read after restart: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("restored content = %q, want %q", got, content)
	}
}

func TestSessionRestoresDuplicateMetadataReferences(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentPath, _ := buildSingleFileTorrent(t, dataDir, work, "payload.bin", []byte("duplicate restart"))
	torrentBytes, err := os.ReadFile(torrentPath)
	if err != nil {
		t.Fatalf("read torrent: %v", err)
	}

	first, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	commitMetadata(t, first, "one.torrent", torrentBytes)
	commitMetadata(t, first, "two.torrent", torrentBytes)
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer func() {
		if err := second.Close(context.Background()); err != nil {
			t.Errorf("second Close: %v", err)
		}
	}()
	if len(second.List()) != 1 {
		t.Fatalf("List after restart = %d, want 1", len(second.List()))
	}
	if err := second.RemoveMetadata(context.Background(), "one.torrent"); err != nil {
		t.Fatalf("remove first restored metadata: %v", err)
	}
	if len(second.List()) != 1 {
		t.Fatalf("List after first restored remove = %d, want 1", len(second.List()))
	}
	if err := second.RemoveMetadata(context.Background(), "two.torrent"); err != nil {
		t.Fatalf("remove second restored metadata: %v", err)
	}
	if len(second.List()) != 0 {
		t.Fatalf("List after second restored remove = %d, want 0", len(second.List()))
	}
}

func TestSessionMetadataRescanIgnoresSymlinksAndTemporaryEntries(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", []byte("rescan entries"))
	torrentBytes, err := os.ReadFile(torrentPath)
	if err != nil {
		t.Fatalf("read torrent: %v", err)
	}
	metadataDir := filepath.Join(testTorrentDir(t, dataDir), ".metadata")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("make metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "valid.torrent"), torrentBytes, 0o644); err != nil {
		t.Fatalf("write valid metadata: %v", err)
	}
	if err := os.Symlink(torrentPath, filepath.Join(metadataDir, "link.torrent")); err != nil {
		t.Fatalf("write metadata symlink: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, ".torrentfs-write.tmp"), torrentBytes, 0o644); err != nil {
		t.Fatalf("write metadata temporary: %v", err)
	}
	if err := os.Mkdir(filepath.Join(metadataDir, "directory.torrent"), 0o755); err != nil {
		t.Fatalf("write metadata directory: %v", err)
	}

	sess, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	files := sess.MetadataFiles()
	if len(files) != 1 || files[0].Name != "valid.torrent" {
		t.Fatalf("MetadataFiles = %+v, want only valid.torrent", files)
	}
	if len(sess.List()) != 1 {
		t.Fatalf("List = %d, want 1", len(sess.List()))
	}
	if _, ok := sess.Torrent(hash); !ok {
		t.Fatal("valid metadata torrent missing")
	}
}

func TestSessionMetadataRescanRejectsCorruptTorrent(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	metadataDir := filepath.Join(testTorrentDir(t, dataDir), ".metadata")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("make metadata dir: %v", err)
	}
	name := "broken.torrent"
	if err := os.WriteFile(filepath.Join(metadataDir, name), []byte("not a torrent"), 0o644); err != nil {
		t.Fatalf("write corrupt metadata: %v", err)
	}

	_, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
	if err == nil {
		t.Fatal("New corrupt metadata succeeded")
	}
	if !strings.Contains(err.Error(), name) {
		t.Fatalf("New corrupt metadata error = %q, want filename", err)
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
	sess, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

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
	sess, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

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
	entries, err := os.ReadDir(filepath.Join(testTorrentDir(t, dataDir), ".metadata"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files remain after failed commit: %+v", entries)
	}
}

func TestSessionCloseIsIdempotentAndRejectsNewOperations(t *testing.T) {
	work := t.TempDir()
	sess, err := session.New(testConfig(filepath.Join(work, "data")), testTorrentDir(t, filepath.Join(work, "data")))
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

// TestSessionLayoutFlagMatchesMetainfo pins the layout classifier the mount
// uses: an info without directory structure is single-file (one direct regular
// file), and a multi-file info is a directory even when it holds one file.
func TestSessionLayoutFlagMatchesMetainfo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	singlePath, singleHash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", []byte("single layout"))
	multiFiles := map[string][]byte{"only.bin": []byte("one file but a directory")}
	multiPath, multiHash, _ := buildMultiFileTorrent(t, dataDir, work, "multi", multiFiles)

	sess, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: singlePath}); err != nil {
		t.Fatalf("AddTorrent(single): %v", err)
	}
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: multiPath}); err != nil {
		t.Fatalf("AddTorrent(multi): %v", err)
	}

	byHash := map[string]filesystem.TorrentView{}
	for _, v := range sess.Torrents() {
		byHash[v.Hash.HexString()] = v
	}
	if v := byHash[singleHash.HexString()]; !v.SingleFile {
		t.Fatalf("single-file view = %+v, want SingleFile", v)
	}
	if v := byHash[multiHash.HexString()]; v.SingleFile {
		t.Fatalf("one-file multi-file view = %+v, want directory layout", v)
	}
}

// TestSessionStatsControlAnchor checks the physical anchor of the stats
// control namespace is created next to the metadata sidecar.
func TestSessionStatsControlAnchor(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := testTorrentDir(t, dataDir)

	sess, err := session.New(testConfig(dataDir), torrentsDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	for _, name := range []string{".metadata", ".stats"} {
		info, err := os.Stat(filepath.Join(torrentsDir, name))
		if err != nil || !info.IsDir() {
			t.Fatalf("%s = (%v, %v), want directory", name, info, err)
		}
	}
}
