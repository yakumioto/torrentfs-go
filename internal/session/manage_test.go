package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

func newManageSession(t *testing.T, dataDir string) *session.Session {
	t.Helper()
	sess, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return sess
}

func waitOperation(t *testing.T, sess *session.Session, id string) session.Operation {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		op, ok := sess.Operation(id)
		if ok && op.State != session.StateDeleting {
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s never reached a terminal state", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func seedPayload(t *testing.T, dataDir string, hash metainfo.Hash, name string, content []byte) string {
	t.Helper()
	dir := payloadDir(dataDir, hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make payload dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	return dir
}

func registryPath(dataDir string, hash metainfo.Hash) string {
	return filepath.Join(dataDir, "state", hash.HexString()+".json")
}

func writeRegistry(t *testing.T, dataDir string, hash metainfo.Hash, state string, purge bool, opID string) {
	t.Helper()
	now := time.Now().UTC()
	entry := map[string]any{
		"id":              hash.HexString(),
		"info_hash":       hash.HexString(),
		"name":            "payload.bin",
		"state":           state,
		"purge_requested": purge,
		"operation_id":    opID,
		"created_at":      now,
		"updated_at":      now,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}
	dir := filepath.Join(dataDir, "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make state dir: %v", err)
	}
	if err := os.WriteFile(registryPath(dataDir, hash), data, 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

func TestAddListDeleteKeepsPayloadByDefault(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	content := []byte(strings.Repeat("payload-", 512))
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	seedPayload(t, dataDir, hash, "payload.bin", content)

	sess := newManageSession(t, dataDir)
	view, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes})
	if err != nil {
		t.Fatalf("AddTorrentAndPersist: %v", err)
	}
	if view.InfoHash != hash.HexString() || view.ID != hash.HexString() {
		t.Fatalf("view ids = %q/%q, want %q", view.ID, view.InfoHash, hash.HexString())
	}
	if view.TotalBytes != int64(len(content)) {
		t.Fatalf("view total = %d, want %d", view.TotalBytes, len(content))
	}
	if view.CreatedAt.IsZero() {
		t.Fatal("view created_at is zero")
	}

	list := sess.ListTorrents()
	if len(list) != 1 || list[0].InfoHash != hash.HexString() {
		t.Fatalf("ListTorrents = %+v, want one entry for %s", list, hash.HexString())
	}

	op, err := sess.DeleteTorrent(ctx, hash.HexString(), false)
	if err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}
	final := waitOperation(t, sess, op.ID)
	if final.State != session.StateDeleted {
		t.Fatalf("delete state = %s (%s), want deleted", final.State, final.Error)
	}
	if _, err := os.Stat(payloadDir(dataDir, hash)); err != nil {
		t.Fatalf("payload removed with purge_data=false: %v", err)
	}
	if _, err := sess.TorrentViewFor(hash.HexString()); !errors.Is(err, session.ErrUnknownTorrent) {
		t.Fatalf("TorrentViewFor after delete = %v, want ErrUnknownTorrent", err)
	}
	if views := sess.ListTorrents(); len(views) != 0 {
		t.Fatalf("ListTorrents after delete = %+v, want empty", views)
	}
	metaPath := filepath.Join(testTorrentDir(t, dataDir), ".metadata", hash.HexString()+".torrent")
	if _, err := os.Stat(metaPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed metainfo survived delete: %v", err)
	}
	if _, err := os.Stat(registryPath(dataDir, hash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state sidecar survived delete: %v", err)
	}
}

func TestAddTorrentDeduplicatesByInfoHash(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", []byte("dedupe payload"), nil)
	sess := newManageSession(t, dataDir)

	first, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes})
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	second, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes})
	if err != nil {
		t.Fatalf("second add: %v", err)
	}
	if first.InfoHash != second.InfoHash {
		t.Fatalf("dedupe hashes differ: %s vs %s", first.InfoHash, second.InfoHash)
	}
	if views := sess.ListTorrents(); len(views) != 1 {
		t.Fatalf("ListTorrents = %d entries, want 1", len(views))
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered by hash")
	}
	if st.InfoHash() != hash {
		t.Fatalf("registered hash = %s, want %s", st.InfoHash(), hash)
	}
}

func TestRepeatedDeleteReturnsSameOperation(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", []byte("idempotent delete"), nil)
	sess := newManageSession(t, dataDir)
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("add: %v", err)
	}

	first, err := sess.DeleteTorrent(ctx, hash.HexString(), false)
	if err != nil {
		t.Fatalf("first delete: %v", err)
	}
	second, err := sess.DeleteTorrent(ctx, hash.HexString(), false)
	if err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("repeated delete operation ids differ: %s vs %s", first.ID, second.ID)
	}
	if final := waitOperation(t, sess, first.ID); final.State != session.StateDeleted {
		t.Fatalf("delete state = %s, want deleted", final.State)
	}
}

func TestDeleteWithPurgeRemovesOnlyItsOwnDirectory(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")

	contentA := []byte("torrent a payload")
	bytesA, hashA := buildSingleFileTorrentBytes(t, "a.bin", contentA, nil)
	seedPayload(t, dataDir, hashA, "a.bin", contentA)

	contentB := []byte("torrent b payload")
	bytesB, hashB := buildSingleFileTorrentBytes(t, "b.bin", contentB, nil)
	dirB := seedPayload(t, dataDir, hashB, "b.bin", contentB)

	sess := newManageSession(t, dataDir)
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: bytesA}); err != nil {
		t.Fatalf("add a: %v", err)
	}
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: bytesB}); err != nil {
		t.Fatalf("add b: %v", err)
	}

	op, err := sess.DeleteTorrent(ctx, hashA.HexString(), true)
	if err != nil {
		t.Fatalf("delete a: %v", err)
	}
	if final := waitOperation(t, sess, op.ID); final.State != session.StateDeleted {
		t.Fatalf("delete a state = %s (%s), want deleted", final.State, final.Error)
	}
	if _, err := os.Stat(payloadDir(dataDir, hashA)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("torrent a payload survived purge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirB, "b.bin")); err != nil {
		t.Fatalf("torrent b payload was disturbed: %v", err)
	}
}

func TestPurgeRefusesUnmanagedSymlinkTarget(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", []byte("symlink payload"), nil)
	sess := newManageSession(t, dataDir)
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Replace the torrent's payload directory with a symlink to data outside
	// the managed root. Purging must refuse rather than follow the link.
	outside := filepath.Join(work, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("make outside dir: %v", err)
	}
	precious := filepath.Join(outside, "precious.bin")
	if err := os.WriteFile(precious, []byte("do not delete"), 0o644); err != nil {
		t.Fatalf("write precious: %v", err)
	}
	target := payloadDir(dataDir, hash)
	if err := os.Symlink(outside, target); err != nil {
		t.Fatalf("make symlink: %v", err)
	}

	op, err := sess.DeleteTorrent(ctx, hash.HexString(), true)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	final := waitOperation(t, sess, op.ID)
	if final.State != session.StateDeleteFailed {
		t.Fatalf("delete state = %s (%s), want delete_failed", final.State, final.Error)
	}
	if !strings.Contains(final.Error, "refus") {
		t.Fatalf("delete error = %q, want a refusal", final.Error)
	}
	if _, err := os.Stat(precious); err != nil {
		t.Fatalf("purge followed the symlink and removed data: %v", err)
	}
	// The task stays visible while failed so the deletion can be retried.
	views := sess.ListTorrents()
	if len(views) != 1 || views[0].State != session.StateDeleteFailed {
		t.Fatalf("ListTorrents = %+v, want one delete_failed entry", views)
	}

	// Retrying without purge succeeds and leaves the payload alone.
	retry, err := sess.DeleteTorrent(ctx, hash.HexString(), false)
	if err != nil {
		t.Fatalf("retry delete: %v", err)
	}
	if retry.ID != op.ID {
		t.Fatalf("retry operation id = %s, want the original %s", retry.ID, op.ID)
	}
	if final := waitOperation(t, sess, retry.ID); final.State != session.StateDeleted {
		t.Fatalf("retry state = %s (%s), want deleted", final.State, final.Error)
	}
}

func TestDeleteRefusedForDirectorySourcedTorrent(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := filepath.Join(work, "torrents")
	if err := os.MkdirAll(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	content := []byte("directory sourced payload")
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	userTorrent := filepath.Join(torrentsDir, "payload.bin.torrent")
	if err := os.WriteFile(userTorrent, torrentBytes, 0o644); err != nil {
		t.Fatalf("write user torrent: %v", err)
	}
	payload := seedPayload(t, dataDir, hash, "payload.bin", content)

	sess := newManageSession(t, dataDir)
	if _, ok := sess.Torrent(hash); !ok {
		t.Fatal("torrent was not registered from the torrents directory")
	}

	_, err := sess.DeleteTorrent(ctx, hash.HexString(), true)
	if !errors.Is(err, session.ErrExternalReference) {
		t.Fatalf("delete = %v, want ErrExternalReference", err)
	}
	if _, ok := sess.Torrent(hash); !ok {
		t.Fatal("refused delete still removed the torrent")
	}
	if _, err := os.Stat(filepath.Join(payload, "payload.bin")); err != nil {
		t.Fatalf("refused delete disturbed the payload: %v", err)
	}
	if _, err := os.Stat(userTorrent); err != nil {
		t.Fatalf("refused delete removed the user's .torrent: %v", err)
	}
}

func TestAddRejectedWhileDeleteFailed(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", []byte("blocked"), nil)
	writeRegistry(t, dataDir, hash, string(session.StateDeleteFailed), false, "op-existing")

	sess := newManageSession(t, dataDir)
	_, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes})
	if !errors.Is(err, session.ErrDeleting) {
		t.Fatalf("add during delete_failed = %v, want ErrDeleting", err)
	}
}

func TestResumeCompletesInterruptedDeletion(t *testing.T) {
	for _, purge := range []bool{false, true} {
		name := "keep"
		if purge {
			name = "purge"
		}
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			dataDir := filepath.Join(work, "data")
			torrentsDir := filepath.Join(work, "torrents")
			content := []byte("interrupted deletion payload")
			torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)

			metadataDir := filepath.Join(torrentsDir, ".metadata")
			if err := os.MkdirAll(metadataDir, 0o755); err != nil {
				t.Fatalf("make metadata dir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(metadataDir, hash.HexString()+".torrent"), torrentBytes, 0o644); err != nil {
				t.Fatalf("write metadata: %v", err)
			}
			payload := seedPayload(t, dataDir, hash, "payload.bin", content)
			writeRegistry(t, dataDir, hash, string(session.StateDeleting), purge, "op-resume")

			sess, err := session.New(testConfig(dataDir), torrentsDir)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer func() {
				if err := sess.Close(context.Background()); err != nil {
					t.Errorf("Close: %v", err)
				}
			}()

			// resumeDeletions runs before New returns, so the interruption is
			// already resolved.
			if views := sess.ListTorrents(); len(views) != 0 {
				t.Fatalf("ListTorrents after resume = %+v, want empty", views)
			}
			if _, err := sess.TorrentViewFor(hash.HexString()); !errors.Is(err, session.ErrUnknownTorrent) {
				t.Fatalf("TorrentViewFor after resume = %v, want ErrUnknownTorrent", err)
			}
			if _, err := os.Stat(registryPath(dataDir, hash)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("state sidecar survived resume: %v", err)
			}
			_, statErr := os.Stat(payload)
			if purge && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("payload survived resume with purge_data=true: %v", statErr)
			}
			if !purge && statErr != nil {
				t.Fatalf("payload removed by resume with purge_data=false: %v", statErr)
			}
			if op, ok := sess.Operation("op-resume"); !ok || op.State != session.StateDeleted {
				t.Fatalf("resumed operation = %+v (%v), want deleted", op, ok)
			}
		})
	}
}

func TestDeleteReleasesGoroutinesAndFileDescriptors(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("file descriptor accounting requires /proc")
	}
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	content := []byte(strings.Repeat("leak-", 1024))
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	seedPayload(t, dataDir, hash, "payload.bin", content)

	baselineGoroutines := runtime.NumGoroutine()
	baselineFDs := openFDCount(t)

	settle := func() (int, int) {
		deadline := time.Now().Add(5 * time.Second)
		var goroutines, fds int
		for {
			runtime.GC()
			goroutines = runtime.NumGoroutine()
			fds = openFDCount(t)
			if time.Now().After(deadline) {
				return goroutines, fds
			}
			if goroutines <= baselineGoroutines+2 && fds <= baselineFDs+2 {
				return goroutines, fds
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	sess, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	view, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	op, err := sess.DeleteTorrent(ctx, view.ID, false)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if final := waitOperation(t, sess, op.ID); final.State != session.StateDeleted {
		t.Fatalf("delete state = %s (%s), want deleted", final.State, final.Error)
	}
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	goroutines, fds := settle()
	if goroutines > baselineGoroutines+2 {
		t.Fatalf("goroutines after add/delete/close = %d, baseline %d", goroutines, baselineGoroutines)
	}
	if fds > baselineFDs+2 {
		t.Fatalf("open file descriptors after add/delete/close = %d, baseline %d", fds, baselineFDs)
	}
}

func TestDeleteMagnetInAddingStateStopsMetadataFetch(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	hexHash := strings.Repeat("b", 40)
	hash := metainfo.NewHashFromHex(hexHash)
	magnet := "magnet:?xt=urn:btih:" + hexHash + "&dn=adding-task"

	sess := newManageSession(t, dataDir)
	view, err := sess.AddTorrentAndPersist(ctx, session.Source{MagnetURI: magnet})
	if err != nil {
		t.Fatalf("add magnet: %v", err)
	}
	if view.State != session.StateAdding {
		t.Fatalf("state = %s, want adding", view.State)
	}
	if got := sess.PendingMetadataFetches(); got != 1 {
		t.Fatalf("pending metadata fetches = %d, want 1", got)
	}

	op, err := sess.DeleteTorrent(ctx, hexHash, false)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if final := waitOperation(t, sess, op.ID); final.State != session.StateDeleted {
		t.Fatalf("delete state = %s (%s), want deleted", final.State, final.Error)
	}
	// The worker is cancelled and waited on, so nothing is left tracking it.
	if got := sess.PendingMetadataFetches(); got != 0 {
		t.Fatalf("pending metadata fetches after delete = %d, want 0", got)
	}
	if _, ok := sess.Torrent(hash); ok {
		t.Fatal("magnet torrent still registered after delete")
	}
	metaPath := filepath.Join(testTorrentDir(t, dataDir), ".metadata", hexHash+".torrent")
	if _, err := os.Stat(metaPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata sidecar present after delete: %v", err)
	}
	if _, err := os.Stat(registryPath(dataDir, hash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state sidecar present after delete: %v", err)
	}
}

func TestLateMetadataWriteRefusedWhileDeleteFailed(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", []byte("late write"), nil)
	sess := newManageSession(t, dataDir)
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Force a failed delete by pointing the payload directory at a symlink.
	outside := filepath.Join(work, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("make outside dir: %v", err)
	}
	if err := os.Symlink(outside, payloadDir(dataDir, hash)); err != nil {
		t.Fatalf("make symlink: %v", err)
	}
	op, err := sess.DeleteTorrent(ctx, hash.HexString(), true)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if final := waitOperation(t, sess, op.ID); final.State != session.StateDeleteFailed {
		t.Fatalf("delete state = %s (%s), want delete_failed", final.State, final.Error)
	}

	// A late metadata write must be refused even with a live context, so it
	// can never recreate the sidecar and revive the task.
	err = sess.WriteMetadataForTest(context.Background(), hash, torrentBytes)
	if !errors.Is(err, session.ErrDeleting) {
		t.Fatalf("late metadata write = %v, want ErrDeleting", err)
	}
	metaPath := filepath.Join(testTorrentDir(t, dataDir), ".metadata", hash.HexString()+".torrent")
	if _, err := os.Stat(metaPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("late metadata write recreated the sidecar: %v", err)
	}
}

func TestLateMetadataFetchDoesNotResurrectDeletedTorrent(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", []byte("resurrect"), nil)
	sess := newManageSession(t, dataDir)
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("add: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}

	// Hold the metadata worker mid-flight so the deletion races a write that
	// lands after the delete has begun.
	gate := make(chan struct{})
	restore := session.SetMetadataFetchHook(func(metainfo.Hash) { <-gate })
	defer restore()
	sess.StartMetadataFetch(st)
	if got := sess.PendingMetadataFetches(); got != 1 {
		t.Fatalf("pending metadata fetches = %d, want 1", got)
	}

	op, err := sess.DeleteTorrent(ctx, hash.HexString(), false)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	close(gate)
	if final := waitOperation(t, sess, op.ID); final.State != session.StateDeleted {
		t.Fatalf("delete state = %s (%s), want deleted", final.State, final.Error)
	}
	if _, ok := sess.Torrent(hash); ok {
		t.Fatal("late metadata write resurrected the deleted torrent")
	}
	if got := sess.PendingMetadataFetches(); got != 0 {
		t.Fatalf("pending metadata fetches after delete = %d, want 0", got)
	}
	if views := sess.ListTorrents(); len(views) != 0 {
		t.Fatalf("ListTorrents = %+v, want empty", views)
	}
	metaPath := filepath.Join(testTorrentDir(t, dataDir), ".metadata", hash.HexString()+".torrent")
	if _, err := os.Stat(metaPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("late metadata fetch recreated the sidecar: %v", err)
	}
}

func TestResumeDeletionClearsMetadataIndexAndAllowsRepersist(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentsDir := filepath.Join(work, "torrents")
	content := []byte("resume metadata index")
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)

	metadataDir := filepath.Join(torrentsDir, ".metadata")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("make metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, hash.HexString()+".torrent"), torrentBytes, 0o644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	seedPayload(t, dataDir, hash, "payload.bin", content)
	writeRegistry(t, dataDir, hash, string(session.StateDeleting), false, "op-resume-index")

	sess, err := session.New(testConfig(dataDir), torrentsDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	if _, err := sess.TorrentViewFor(hash.HexString()); !errors.Is(err, session.ErrUnknownTorrent) {
		t.Fatalf("torrent survived resume: %v", err)
	}
	// The in-memory metadata index must have been cleared too: a fresh add of
	// the same hash has to persist its metainfo again instead of silently
	// skipping it.
	if _, err := sess.AddTorrentAndPersist(context.Background(), session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("re-add after resume: %v", err)
	}
	if _, err := os.Stat(filepath.Join(metadataDir, hash.HexString()+".torrent")); err != nil {
		t.Fatalf("re-added torrent was not persisted (stale metadata index): %v", err)
	}
}

func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	return len(entries)
}
