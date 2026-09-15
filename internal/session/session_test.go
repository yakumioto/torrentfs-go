package session_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// persistManaged registers metainfo through the management path, which also
// publishes its canonical <info-hash>.torrent into the internal metadata
// directory.
func persistManaged(t *testing.T, sess *session.Session, data []byte) {
	t.Helper()
	if _, err := sess.AddTorrentAndPersist(context.Background(), session.Source{Metainfo: data}); err != nil {
		t.Fatalf("AddTorrentAndPersist: %v", err)
	}
}

func TestSessionTorrentNamedMetadataIsPlainData(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	content := []byte("torrent named metadata")
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "metadata", content)

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

	// The internal metadata directory is a sibling of the torrent data, not a
	// name reserved inside the mount root.
	metadataDir := filepath.Join(torrentsDir, ".metadata")
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

	// Persisting metainfo for the same hash publishes the canonical file.
	torrentBytes, err := os.ReadFile(torrentPath)
	if err != nil {
		t.Fatalf("read torrent: %v", err)
	}
	persistManaged(t, sess, torrentBytes)
	if _, err := os.Stat(filepath.Join(metadataDir, hash.HexString()+".torrent")); err != nil {
		t.Fatalf("canonical metadata file missing: %v", err)
	}
}

func TestSessionDeleteRemovesAllInternalMetadataReferences(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	content := []byte("legacy metadata references")
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", content)
	torrentBytes, err := os.ReadFile(torrentPath)
	if err != nil {
		t.Fatalf("read torrent: %v", err)
	}

	torrentsDir := testTorrentDir(t, dataDir)
	metadataDir := filepath.Join(torrentsDir, ".metadata")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("make metadata dir: %v", err)
	}
	// A legacy FUSE-era managed source: any valid *.torrent name, not the
	// canonical <info-hash>.torrent.
	legacy := filepath.Join(metadataDir, "saved.torrent")
	if err := os.WriteFile(legacy, torrentBytes, 0o644); err != nil {
		t.Fatalf("write legacy metadata: %v", err)
	}

	first, err := session.New(testConfig(dataDir), torrentsDir)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if _, ok := first.Torrent(hash); !ok {
		t.Fatal("legacy managed metadata was not restored on startup")
	}
	persistManaged(t, first, torrentBytes)
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := session.New(testConfig(dataDir), torrentsDir)
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer func() {
		if err := second.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	op, err := second.DeleteTorrent(ctx, hash.HexString(), false)
	if err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}
	waitOperationDone(t, ctx, second, op.ID)
	for _, name := range []string{"saved.torrent", hash.HexString() + ".torrent"} {
		if _, err := os.Stat(filepath.Join(metadataDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("internal metadata %q still present after delete", name)
		}
	}
	if err := second.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// A restart must not resurrect the deleted task from a leftover source.
	third, err := session.New(testConfig(dataDir), torrentsDir)
	if err != nil {
		t.Fatalf("third New: %v", err)
	}
	defer func() {
		if err := third.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if _, ok := third.Torrent(hash); ok {
		t.Fatal("deleted torrent was resurrected from a leftover metadata source")
	}
}

// waitOperationDone blocks until the deletion operation reaches a terminal
// state.
func waitOperationDone(t *testing.T, ctx context.Context, sess *session.Session, opID string) {
	t.Helper()
	for {
		op, ok := sess.Operation(opID)
		if ok && op.State != session.StateDeleting {
			if op.State != session.StateDeleted {
				t.Fatalf("delete operation state = %s (%s), want deleted", op.State, op.Error)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("delete operation never finished: %v", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
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

	torrentsDir := testTorrentDir(t, dataDir)
	first, err := session.New(testConfig(dataDir), torrentsDir)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	persistManaged(t, first, torrentBytes)
	st, ok := first.Torrent(hash)
	if !ok {
		t.Fatal("first session did not register the managed torrent")
	}
	waitComplete(t, ctx, st)
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(torrentsDir, ".metadata", hash.HexString()+".torrent")); err != nil {
		t.Fatalf("canonical metadata file missing: %v", err)
	}

	second, err := session.New(testConfig(dataDir), torrentsDir)
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer func() {
		if err := second.Close(context.Background()); err != nil {
			t.Errorf("second Close: %v", err)
		}
	}()

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

// TestSessionPendingMagnetIntentSurvivesRestart checks an unresolved magnet is
// persisted as a durable intent and restored after a restart, instead of being
// silently dropped when the process exits before the metainfo arrives.
func TestSessionPendingMagnetIntentSurvivesRestart(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := testTorrentDir(t, dataDir)
	magnet := "magnet:?xt=urn:btih:" + strings.Repeat("c", 40)

	first, err := session.New(testConfig(dataDir), torrentsDir)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	view, err := first.AddTorrentAndPersist(ctx, session.Source{MagnetURI: magnet})
	if err != nil {
		t.Fatalf("AddTorrentAndPersist(magnet): %v", err)
	}
	if view.State != session.StateAdding {
		t.Fatalf("magnet state = %s, want adding", view.State)
	}
	intent := filepath.Join(torrentsDir, ".metadata", view.ID+".magnet")
	if _, err := os.Stat(intent); err != nil {
		t.Fatalf("pending magnet intent missing: %v", err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := session.New(testConfig(dataDir), torrentsDir)
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer func() {
		if err := second.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if _, err := os.Stat(intent); err != nil {
		t.Fatalf("pending magnet intent lost after restart: %v", err)
	}
	found := false
	for _, listed := range second.ListTorrents() {
		if listed.ID == view.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("pending magnet task missing after restart: %+v", second.ListTorrents())
	}

	op, err := second.DeleteTorrent(ctx, view.ID, false)
	if err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}
	waitOperationDone(t, ctx, second, op.ID)
	if _, err := os.Stat(intent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending magnet intent survived its deletion: %v", err)
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
	if len(sess.List()) != 1 {
		t.Fatalf("List = %d, want only the valid metadata torrent", len(sess.List()))
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

// TestSessionDuplicateInternalReferencesShareOneTask checks two internal
// metadata sources for the same info hash register a single task and that
// deleting it removes every source, not just the canonical one.
func TestSessionDuplicateInternalReferencesShareOneTask(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", []byte("duplicate metadata"))
	torrentBytes, err := os.ReadFile(torrentPath)
	if err != nil {
		t.Fatalf("read torrent: %v", err)
	}
	torrentsDir := testTorrentDir(t, dataDir)
	metadataDir := filepath.Join(torrentsDir, ".metadata")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("make metadata dir: %v", err)
	}
	legacy := filepath.Join(metadataDir, "saved.torrent")
	if err := os.WriteFile(legacy, torrentBytes, 0o644); err != nil {
		t.Fatalf("write legacy metadata: %v", err)
	}

	sess, err := session.New(testConfig(dataDir), torrentsDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	persistManaged(t, sess, torrentBytes)
	if len(sess.List()) != 1 {
		t.Fatalf("List with duplicate internal references = %d, want 1", len(sess.List()))
	}

	op, err := sess.DeleteTorrent(ctx, hash.HexString(), false)
	if err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}
	waitOperationDone(t, ctx, sess, op.ID)
	if len(sess.List()) != 0 {
		t.Fatalf("List after delete = %d, want 0", len(sess.List()))
	}
	for _, name := range []string{"saved.torrent", hash.HexString() + ".torrent"} {
		if _, err := os.Stat(filepath.Join(metadataDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("internal metadata %q survived the deletion", name)
		}
	}
}

// TestSessionIgnoresLegacyStatsDirectory checks the session neither creates
// <torrents-dir>/.stats nor removes a legacy one, since piece state comes from
// the running torrent client and not from that directory.
func TestSessionIgnoresLegacyStatsDirectory(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := testTorrentDir(t, dataDir)
	legacyStats := filepath.Join(torrentsDir, ".stats")
	if err := os.MkdirAll(legacyStats, 0o755); err != nil {
		t.Fatalf("make legacy stats dir: %v", err)
	}
	marker := filepath.Join(legacyStats, "leftover.txt")
	if err := os.WriteFile(marker, []byte("legacy"), 0o644); err != nil {
		t.Fatalf("write legacy stats marker: %v", err)
	}

	sess, err := session.New(testConfig(dataDir), torrentsDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("legacy stats contents were removed: %v", err)
	}

	// A fresh directory must not gain a .stats anchor either.
	freshDataDir := filepath.Join(work, "fresh-data")
	freshTorrentsDir := filepath.Join(work, "fresh-torrents")
	if err := os.MkdirAll(freshTorrentsDir, 0o755); err != nil {
		t.Fatalf("make fresh torrents dir: %v", err)
	}
	fresh, err := session.New(testConfig(freshDataDir), freshTorrentsDir)
	if err != nil {
		t.Fatalf("fresh New: %v", err)
	}
	defer func() {
		if err := fresh.Close(context.Background()); err != nil {
			t.Errorf("fresh Close: %v", err)
		}
	}()
	if _, err := os.Stat(filepath.Join(freshTorrentsDir, ".stats")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session created a .stats directory: %v", err)
	}
}

// TestSessionUploadRollsBackWhenPersistenceFails checks an upload whose
// canonical metainfo cannot be published leaves no ghost task behind: the
// runtime registration is rolled back so the API never reports success for a
// torrent that would vanish on restart.
func TestSessionUploadRollsBackWhenPersistenceFails(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", []byte("rollback payload"))
	torrentBytes, err := os.ReadFile(torrentPath)
	if err != nil {
		t.Fatalf("read torrent: %v", err)
	}
	torrentsDir := testTorrentDir(t, dataDir)
	metadataDir := filepath.Join(torrentsDir, ".metadata")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("make metadata dir: %v", err)
	}
	// A directory occupying the canonical name makes publication fail.
	blocked := filepath.Join(metadataDir, hash.HexString()+".torrent")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatalf("block canonical metadata path: %v", err)
	}

	sess, err := session.New(testConfig(dataDir), torrentsDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes}); err == nil {
		t.Fatal("AddTorrentAndPersist succeeded despite an unpublished canonical metainfo")
	}
	if _, ok := sess.Torrent(hash); ok {
		t.Fatal("failed upload left a ghost torrent registered")
	}
	if len(sess.ListTorrents()) != 0 {
		t.Fatalf("failed upload left tasks behind: %+v", sess.ListTorrents())
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

// TestSessionMetadataRootIsInsideTorrentsDir checks the internal metadata
// directory is created as an implementation detail of the torrents directory
// and carries no stats anchor next to it.
func TestSessionMetadataRootIsInsideTorrentsDir(t *testing.T) {
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

	info, err := os.Stat(filepath.Join(torrentsDir, ".metadata"))
	if err != nil || !info.IsDir() {
		t.Fatalf(".metadata = (%v, %v), want directory", info, err)
	}
	if _, err := os.Stat(filepath.Join(torrentsDir, ".stats")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".stats = %v, want not created", err)
	}
}
