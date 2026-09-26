package session_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

// favoriteRegistryHash builds a distinct, syntactically valid info hash per seed
// for fixtures whose state never restores a torrent handle.
func favoriteRegistryHash(seed byte) metainfo.Hash {
	var hash metainfo.Hash
	for i := range hash {
		hash[i] = seed
	}
	return hash
}

// writeAgedTorrent publishes a real single-file torrent plus a sidecar carrying
// an explicit created_at and favorite flag. That is how the age-based prune
// cases get deterministic "old" torrents without waiting on a real clock, and
// the final metainfo is what a restored registry entry requires.
func writeAgedTorrent(t *testing.T, torrentsDir, name string, createdAt time.Time, favorite bool) metainfo.Hash {
	t.Helper()
	torrentBytes, hash := buildSingleFileTorrentBytes(t, name+".bin", []byte("payload-of-"+name), nil)
	if err := os.WriteFile(filepath.Join(torrentsDir, hash.HexString()+".torrent"), torrentBytes, 0o644); err != nil {
		t.Fatalf("write metainfo: %v", err)
	}
	writeFavoriteRegistry(t, torrentsDir, hash, string(session.StateReady), createdAt, favorite)
	return hash
}

// blockRegistryWrites makes the next registry write for one hash fail: the
// atomic publish ends in a rename, and a directory cannot be replaced by a
// rename of a plain file. The entry stays loaded in memory, which is how a real
// write failure (read-only or full filesystem, I/O error) reaches the prune
// loop. Call it after the session has opened, since restoring a sidecar
// rewrites it.
func blockRegistryWrites(t *testing.T, torrentsDir string, hash metainfo.Hash) {
	t.Helper()
	path := registryPath(torrentsDir, hash)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove state %s: %v", path, err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("block state %s: %v", path, err)
	}
}

// writeFavoriteRegistry seeds a sidecar with an explicit created_at and favorite
// flag.
func writeFavoriteRegistry(t *testing.T, torrentsDir string, hash metainfo.Hash, state string, createdAt time.Time, favorite bool) {
	t.Helper()
	entry := map[string]any{
		"id":         hash.HexString(),
		"info_hash":  hash.HexString(),
		"name":       "payload.bin",
		"state":      state,
		"created_at": createdAt,
		"updated_at": time.Now().UTC(),
	}
	if favorite {
		entry["favorite"] = true
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("encode state: %v", err)
	}
	dir := filepath.Join(torrentsDir, ".metadata", "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make state dir: %v", err)
	}
	if err := os.WriteFile(registryPath(torrentsDir, hash), data, 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

func TestSetFavoritePersistsAndSurvivesRestart(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	content := []byte(strings.Repeat("payload-", 512))
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	torrentsDir := testTorrentDir(t, dataDir)

	sess := newManageSession(t, torrentsDir)
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("AddTorrentAndPersist: %v", err)
	}
	view, err := sess.SetFavorite(ctx, hash.HexString(), true)
	if err != nil {
		t.Fatalf("SetFavorite: %v", err)
	}
	if !view.Favorite {
		t.Fatal("SetFavorite returned a view with Favorite=false")
	}
	if err := sess.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := newManageSession(t, torrentsDir)
	views := reopened.ListTorrents()
	if len(views) != 1 {
		t.Fatalf("ListTorrents after restart = %d entries, want 1", len(views))
	}
	if !views[0].Favorite {
		t.Fatal("favorite did not survive a session restart")
	}
}

func TestSetFavoriteUnknownTorrent(t *testing.T) {
	ctx := testTimeout(t)
	sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))

	_, err := sess.SetFavorite(ctx, favoriteRegistryHash(0x01).HexString(), true)
	if !errors.Is(err, session.ErrUnknownTorrent) {
		t.Fatalf("SetFavorite unknown = %v, want ErrUnknownTorrent", err)
	}
}

// StateDeleting and StateDeleteFailed share one guard: a torrent the deletion
// path owns cannot be re-marked. A delete_failed entry survives a restart only
// while its cleanup fault is still present, so the fixture forces that fault.
func TestSetFavoriteRejectedWhileDeletionHoldsTorrent(t *testing.T) {
	ctx := testTimeout(t)
	torrentsDir := testTorrentDir(t, filepath.Join(t.TempDir(), "data"))
	hash := favoriteRegistryHash(0x02)
	writeFavoriteRegistry(t, torrentsDir, hash, string(session.StateDeleteFailed), time.Now().UTC(), false)
	restore := session.SetSubtitleCleanupHook(func(metainfo.Hash) error { return errors.New("cleanup still failing") })
	defer restore()

	sess := newManageSession(t, torrentsDir)
	if _, err := sess.SetFavorite(ctx, hash.HexString(), true); !errors.Is(err, session.ErrDeleting) {
		t.Fatalf("SetFavorite while deletion holds the torrent = %v, want ErrDeleting", err)
	}
}

func TestSetFavoriteAllowedWhileAdding(t *testing.T) {
	ctx := testTimeout(t)
	sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))

	added, err := sess.AddTorrentAndPersist(ctx, session.Source{MagnetURI: "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20)})
	if err != nil {
		t.Fatalf("AddTorrentAndPersist: %v", err)
	}
	if added.State != session.StateAdding {
		t.Fatalf("state = %s, want adding", added.State)
	}
	view, err := sess.SetFavorite(ctx, added.ID, true)
	if err != nil {
		t.Fatalf("SetFavorite: %v", err)
	}
	if !view.Favorite {
		t.Fatal("SetFavorite returned Favorite=false for an adding torrent")
	}
}

func TestDirectDeleteIgnoresFavorite(t *testing.T) {
	ctx := testTimeout(t)
	torrentsDir := testTorrentDir(t, filepath.Join(t.TempDir(), "data"))
	hash := writeAgedTorrent(t, torrentsDir, "direct-delete", time.Now().UTC(), true)

	sess := newManageSession(t, torrentsDir)
	op, err := sess.DeleteTorrent(ctx, hash.HexString())
	if err != nil {
		t.Fatalf("DeleteTorrent on a favorite: %v", err)
	}
	if final := waitOperation(t, sess, op.ID); final.State != session.StateDeleted {
		t.Fatalf("direct delete of a favorite = %s (%s), want deleted", final.State, final.Error)
	}
}

func TestPruneKeepsFavoriteOlderThanCutoff(t *testing.T) {
	ctx := testTimeout(t)
	torrentsDir := testTorrentDir(t, filepath.Join(t.TempDir(), "data"))
	hash := writeAgedTorrent(t, torrentsDir, "old-favorite", time.Now().UTC().Add(-40*24*time.Hour), true)

	sess := newManageSession(t, torrentsDir)
	result, err := sess.DeleteUnfavoritedOlderThan(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("DeleteUnfavoritedOlderThan: %v", err)
	}
	if len(result.Operations) != 0 {
		t.Fatalf("prune started %d deletions, want 0", len(result.Operations))
	}
	if result.ExcludedFavorites != 1 {
		t.Fatalf("ExcludedFavorites = %d, want 1", result.ExcludedFavorites)
	}
	view, err := sess.TorrentViewFor(hash.HexString())
	if err != nil {
		t.Fatalf("favorite was removed by prune: %v", err)
	}
	if !view.Favorite {
		t.Fatal("surviving view lost its favorite flag")
	}
}

func TestPruneDeletesOnlyUnfavoritedOlderThanCutoff(t *testing.T) {
	ctx := testTimeout(t)
	torrentsDir := testTorrentDir(t, filepath.Join(t.TempDir(), "data"))
	now := time.Now().UTC()
	oldPlain := writeAgedTorrent(t, torrentsDir, "old-plain", now.Add(-40*24*time.Hour), false)
	oldFavorite := writeAgedTorrent(t, torrentsDir, "old-favorite", now.Add(-40*24*time.Hour), true)
	recentPlain := writeAgedTorrent(t, torrentsDir, "recent-plain", now.Add(-10*24*time.Hour), false)

	sess := newManageSession(t, torrentsDir)
	result, err := sess.DeleteUnfavoritedOlderThan(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("DeleteUnfavoritedOlderThan: %v", err)
	}
	if len(result.Operations) != 1 {
		t.Fatalf("prune started %d deletions, want 1", len(result.Operations))
	}
	if result.ExcludedFavorites != 1 {
		t.Fatalf("ExcludedFavorites = %d, want 1", result.ExcludedFavorites)
	}
	if result.Operations[0].TorrentID != oldPlain.HexString() {
		t.Fatalf("prune deleted %s, want %s", result.Operations[0].TorrentID, oldPlain.HexString())
	}
	if final := waitOperation(t, sess, result.Operations[0].ID); final.State != session.StateDeleted {
		t.Fatalf("pruned operation = %s (%s), want deleted", final.State, final.Error)
	}
	if _, err := sess.TorrentViewFor(oldPlain.HexString()); !errors.Is(err, session.ErrUnknownTorrent) {
		t.Fatalf("old unfavorited torrent survived: %v", err)
	}
	for _, hash := range []metainfo.Hash{oldFavorite, recentPlain} {
		if _, err := sess.TorrentViewFor(hash.HexString()); err != nil {
			t.Fatalf("torrent %s should have survived prune: %v", hash.HexString(), err)
		}
	}
}

// A batch that matched candidates but could not start some of their deletions
// must report that, and must not look like a batch that matched nothing.
func TestPruneReportsCandidatesItCannotStartDeleting(t *testing.T) {
	ctx := testTimeout(t)
	torrentsDir := testTorrentDir(t, filepath.Join(t.TempDir(), "data"))
	now := time.Now().UTC()
	blocked := writeAgedTorrent(t, torrentsDir, "blocked-candidate", now.Add(-40*24*time.Hour), false)
	healthy := writeAgedTorrent(t, torrentsDir, "healthy-candidate", now.Add(-40*24*time.Hour), false)

	sess := newManageSession(t, torrentsDir)
	blockRegistryWrites(t, torrentsDir, blocked)

	result, err := sess.DeleteUnfavoritedOlderThan(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("DeleteUnfavoritedOlderThan: %v", err)
	}

	if len(result.Failures) != 1 || result.Failures[0].TorrentID != blocked.HexString() {
		t.Fatalf("Failures = %+v, want exactly %s", result.Failures, blocked.HexString())
	}
	if result.Failures[0].Error == "" {
		t.Fatal("failure carries no error detail")
	}
	// The batch kept going after the blocked candidate.
	if len(result.Operations) != 1 || result.Operations[0].TorrentID != healthy.HexString() {
		t.Fatalf("Operations = %+v, want exactly %s", result.Operations, healthy.HexString())
	}
	if result.ExcludedFavorites != 0 {
		t.Fatalf("ExcludedFavorites = %d, want 0", result.ExcludedFavorites)
	}
	// A zero-candidate run returns an all-zero result; this one must not.
	var zero session.PruneResult
	if len(result.Operations) == len(zero.Operations) && len(result.Failures) == len(zero.Failures) && result.ExcludedFavorites == zero.ExcludedFavorites {
		t.Fatalf("blocked batch is indistinguishable from a zero-candidate batch: %+v", result)
	}

	if final := waitOperation(t, sess, result.Operations[0].ID); final.State != session.StateDeleted {
		t.Fatalf("healthy candidate = %s (%s), want deleted", final.State, final.Error)
	}
	if _, err := sess.TorrentViewFor(blocked.HexString()); err != nil {
		t.Fatalf("blocked candidate should have been left in place: %v", err)
	}
}

func TestPruneSkipsTorrentsHeldByDeletion(t *testing.T) {
	ctx := testTimeout(t)
	torrentsDir := testTorrentDir(t, filepath.Join(t.TempDir(), "data"))
	hash := favoriteRegistryHash(0x0a)
	writeFavoriteRegistry(t, torrentsDir, hash, string(session.StateDeleteFailed), time.Now().UTC().Add(-40*24*time.Hour), false)

	sess := newManageSession(t, torrentsDir)
	result, err := sess.DeleteUnfavoritedOlderThan(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("DeleteUnfavoritedOlderThan: %v", err)
	}
	if len(result.Operations) != 0 || result.ExcludedFavorites != 0 {
		t.Fatalf("prune touched a torrent held by deletion: %+v", result)
	}
}

func TestPruneRejectsNegativeAge(t *testing.T) {
	ctx := testTimeout(t)
	sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))

	if _, err := sess.DeleteUnfavoritedOlderThan(ctx, -time.Hour); !errors.Is(err, session.ErrInvalidSource) {
		t.Fatalf("negative age = %v, want ErrInvalidSource", err)
	}
}

func TestPruneWithNoCandidatesIsEmptyAndOK(t *testing.T) {
	ctx := testTimeout(t)
	torrentsDir := testTorrentDir(t, filepath.Join(t.TempDir(), "data"))
	hash := writeAgedTorrent(t, torrentsDir, "recent-plain", time.Now().UTC().Add(-time.Hour), false)

	sess := newManageSession(t, torrentsDir)
	result, err := sess.DeleteUnfavoritedOlderThan(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("DeleteUnfavoritedOlderThan: %v", err)
	}
	if len(result.Operations) != 0 || result.ExcludedFavorites != 0 {
		t.Fatalf("prune with no candidates = %+v, want empty", result)
	}
	if _, err := sess.TorrentViewFor(hash.HexString()); err != nil {
		t.Fatalf("recent torrent was removed: %v", err)
	}
}

// A favorite set concurrently with a prune must still be honored: both paths
// serialize on the per-hash lock and the flag is re-read inside that section.
func TestPruneExcludesFavoriteUnderConcurrentSetFavorite(t *testing.T) {
	ctx := testTimeout(t)
	torrentsDir := testTorrentDir(t, filepath.Join(t.TempDir(), "data"))
	hash := writeAgedTorrent(t, torrentsDir, "contended-favorite", time.Now().UTC().Add(-40*24*time.Hour), true)

	sess := newManageSession(t, torrentsDir)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			if _, err := sess.SetFavorite(ctx, hash.HexString(), true); err != nil &&
				!errors.Is(err, session.ErrDeleting) && !errors.Is(err, session.ErrUnknownTorrent) {
				t.Errorf("SetFavorite: %v", err)
				return
			}
		}
	}()
	result, err := sess.DeleteUnfavoritedOlderThan(ctx, 30*24*time.Hour)
	wg.Wait()
	if err != nil {
		t.Fatalf("DeleteUnfavoritedOlderThan: %v", err)
	}
	if len(result.Operations) != 0 {
		t.Fatalf("prune deleted a favorite under contention: %+v", result)
	}
	if result.ExcludedFavorites != 1 {
		t.Fatalf("ExcludedFavorites = %d, want 1", result.ExcludedFavorites)
	}
	if view, err := sess.TorrentViewFor(hash.HexString()); err != nil || !view.Favorite {
		t.Fatalf("favorite did not survive concurrent prune: view=%+v err=%v", view, err)
	}
}
