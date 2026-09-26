package session_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"golang.org/x/sys/unix"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

// multiFileTorrentBytes builds a multi-file torrent and returns its metainfo
// bytes, which is how the management path receives an upload.
func multiFileTorrentBytes(t *testing.T, name string, files map[string][]byte) ([]byte, metainfo.Hash) {
	t.Helper()
	path, hash, _ := buildMultiFileTorrent(t, t.TempDir(), name, files)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read multi-file metainfo: %v", err)
	}
	return data, hash
}

func addTorrentBytes(t *testing.T, sess *session.Session, data []byte) {
	t.Helper()
	if _, err := sess.AddTorrentAndPersist(context.Background(), session.Source{Metainfo: data}); err != nil {
		t.Fatalf("AddTorrentAndPersist: %v", err)
	}
}

func uploadSubtitle(t *testing.T, sess *session.Session, hash metainfo.Hash, videoPath, name, content string) session.SubtitleUploadResponse {
	t.Helper()
	result, err := sess.UploadSubtitle(context.Background(), hash.HexString(), videoPath, name, strings.NewReader(content), 1<<20)
	if err != nil {
		t.Fatalf("UploadSubtitle %s: %v", name, err)
	}
	return result
}

func subtitleTarget(t *testing.T, sess *session.Session, hash metainfo.Hash, videoPath string) session.SubtitleTarget {
	t.Helper()
	status, err := sess.TorrentStatusFor(hash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor: %v", err)
	}
	for _, target := range status.SubtitleTargets {
		if target.VideoPath == videoPath {
			return target
		}
	}
	t.Fatalf("no subtitle target for %q in %+v", videoPath, status.SubtitleTargets)
	return session.SubtitleTarget{}
}

func assertSubtitleCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("upload succeeded, want code %s", want)
	}
	if got := session.SubtitleErrorCode(err); got != want {
		t.Fatalf("error code = %q (%v), want %q", got, err, want)
	}
}

func readSubtitle(t *testing.T, sess *session.Session, hash metainfo.Hash, relPath string) string {
	t.Helper()
	snapshot, err := sess.OpenSubtitle(hash, relPath)
	if err != nil {
		t.Fatalf("OpenSubtitle %q: %v", relPath, err)
	}
	if closer, ok := snapshot.Reader.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}
	data, err := io.ReadAll(io.NewSectionReader(snapshot.Reader, 0, 1<<20))
	if err != nil {
		t.Fatalf("read subtitle %q: %v", relPath, err)
	}
	return string(data)
}

func TestSubtitleTargetsFollowVideoLayout(t *testing.T) {
	data, hash := multiFileTorrentBytes(t, "Season 1", map[string][]byte{
		"Movie.2026.mkv":      []byte("video"),
		"Season 1/E01.mp4":    []byte("episode"),
		"Movie.2026.srt":      []byte("payload subtitle"),
		"readme.txt":          []byte("notes"),
		"cover.jpg":           []byte("image"),
		"Season 1/E01.en.srt": []byte("payload episode subtitle"),
	})
	sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))
	addTorrentBytes(t, sess, data)

	status, err := sess.TorrentStatusFor(hash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor: %v", err)
	}
	if len(status.SubtitleTargets) != 2 {
		t.Fatalf("targets = %+v, want one per supported video", status.SubtitleTargets)
	}
	targets := map[string]session.SubtitleTarget{}
	for _, target := range status.SubtitleTargets {
		targets[target.VideoPath] = target
	}
	movie, ok := targets["Movie.2026.mkv"]
	if !ok {
		t.Fatalf("targets = %+v, want Movie.2026.mkv", status.SubtitleTargets)
	}
	if movie.ExpectedBasename != "Movie.2026" || movie.MountPath != "Season 1/Movie.2026.srt" || !movie.Uploadable || movie.Reason != "" {
		t.Fatalf("movie target = %+v, want an uploadable Movie.2026 target", movie)
	}
	episode, ok := targets["Season 1/E01.mp4"]
	if !ok {
		t.Fatalf("targets = %+v, want Season 1/E01.mp4", status.SubtitleTargets)
	}
	if episode.ExpectedBasename != "E01" || episode.MountPath != "Season 1/Season 1/E01.srt" || !episode.Uploadable {
		t.Fatalf("episode target = %+v, want an uploadable E01 target", episode)
	}
	// Nothing is managed yet, and Files keeps its payload-only meaning.
	if len(status.Subtitles) != 0 {
		t.Fatalf("subtitles = %+v, want none before an upload", status.Subtitles)
	}
}

func TestSubtitleUploadNameAndFormatRules(t *testing.T) {
	data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{
		"Movie.2026.mkv": []byte("video"),
	})
	sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))
	addTorrentBytes(t, sess, data)

	tests := []struct {
		name      string
		videoPath string
		fileName  string
		wantCode  string
	}{
		{"exact basename", "Movie.2026.mkv", "Movie.2026.srt", ""},
		{"language suffix", "Movie.2026.mkv", "Movie.2026.zh-CN.srt", session.SubtitleCodeNameMismatch},
		{"other stem", "Movie.2026.mkv", "other.srt", session.SubtitleCodeNameMismatch},
		{"uppercase extension", "Movie.2026.mkv", "Movie.2026.SRT", session.SubtitleCodeFormatUnsupported},
		{"unsupported extension", "Movie.2026.mkv", "Movie.2026.txt", session.SubtitleCodeFormatUnsupported},
		{"embedded path", "Movie.2026.mkv", "sub/Movie.2026.srt", session.SubtitleCodeFormatUnsupported},
		{"unknown video", "missing.mkv", "missing.srt", session.SubtitleCodeVideoNotFound},
		{"absolute video path", "/etc/passwd", "passwd.srt", session.SubtitleCodeVideoNotFound},
		{"traversal video path", "../Movie.2026.mkv", "Movie.2026.srt", session.SubtitleCodeVideoNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := sess.UploadSubtitle(context.Background(), hash.HexString(), tt.videoPath, tt.fileName, strings.NewReader("x"), 1<<20)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("upload = %v, want success", err)
				}
				return
			}
			assertSubtitleCode(t, err, tt.wantCode)
		})
	}
}

func TestSubtitleCreateReplaceAndCoexist(t *testing.T) {
	data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{"Movie.2026.mkv": []byte("video")})
	sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))
	addTorrentBytes(t, sess, data)

	created := uploadSubtitle(t, sess, hash, "Movie.2026.mkv", "Movie.2026.srt", "one\n")
	if created.Replaced {
		t.Fatal("first upload reported a replacement")
	}
	if created.Path != "Movie.2026.srt" || created.MountPath != "Show/Movie.2026.srt" || created.Format != "srt" || created.Size != 4 {
		t.Fatalf("created = %+v, want a new Show/Movie.2026.srt of 4 bytes", created)
	}

	uploadSubtitle(t, sess, hash, "Movie.2026.mkv", "Movie.2026.ass", "two\n")
	uploadSubtitle(t, sess, hash, "Movie.2026.mkv", "Movie.2026.vtt", "three\n")
	status, err := sess.TorrentStatusFor(hash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor: %v", err)
	}
	if len(status.Subtitles) != 3 {
		t.Fatalf("subtitles = %+v, want three coexisting formats", status.Subtitles)
	}

	replaced := uploadSubtitle(t, sess, hash, "Movie.2026.mkv", "Movie.2026.srt", "replacement\n")
	if !replaced.Replaced {
		t.Fatal("second upload of the same path did not report a replacement")
	}
	if got := readSubtitle(t, sess, hash, "Movie.2026.srt"); got != "replacement\n" {
		t.Fatalf("stored subtitle = %q, want the replacement content", got)
	}
	status, err = sess.TorrentStatusFor(hash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor after replace: %v", err)
	}
	if len(status.Subtitles) != 3 {
		t.Fatalf("subtitles after replace = %+v, want still three", status.Subtitles)
	}
	for _, subtitle := range status.Subtitles {
		if subtitle.Path == "Movie.2026.srt" && subtitle.Size != int64(len("replacement\n")) {
			t.Fatalf("replaced subtitle = %+v, want the new size", subtitle)
		}
	}
}

func TestSubtitleUploadAcceptsEmptyFileAndRejectsOversize(t *testing.T) {
	data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{"Movie.2026.mkv": []byte("video")})
	sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))
	addTorrentBytes(t, sess, data)

	if result := uploadSubtitle(t, sess, hash, "Movie.2026.mkv", "Movie.2026.srt", ""); result.Size != 0 {
		t.Fatalf("empty subtitle size = %d, want 0", result.Size)
	}
	_, err := sess.UploadSubtitle(context.Background(), hash.HexString(), "Movie.2026.mkv", "Movie.2026.vtt", strings.NewReader(strings.Repeat("x", 64)), 8)
	assertSubtitleCode(t, err, session.SubtitleCodeUploadTooLarge)
	if got := readSubtitle(t, sess, hash, "Movie.2026.srt"); got != "" {
		t.Fatalf("stored subtitle = %q, want the empty file untouched by a failed upload", got)
	}
}

func TestSubtitlePayloadConflictIsRefused(t *testing.T) {
	data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{
		"Movie.2026.mkv": []byte("video"),
		"Movie.2026.srt": []byte("payload subtitle"),
	})
	sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))
	addTorrentBytes(t, sess, data)

	target := subtitleTarget(t, sess, hash, "Movie.2026.mkv")
	if !target.Uploadable {
		t.Fatalf("target = %+v, want the video itself uploadable", target)
	}
	_, err := sess.UploadSubtitle(context.Background(), hash.HexString(), "Movie.2026.mkv", "Movie.2026.srt", strings.NewReader("mine"), 1<<20)
	assertSubtitleCode(t, err, session.SubtitleCodePayloadConflict)
	if status, err := sess.TorrentStatusFor(hash.HexString()); err != nil {
		t.Fatalf("TorrentStatusFor: %v", err)
	} else if len(status.Subtitles) != 0 {
		t.Fatalf("subtitles = %+v, want the payload file never adopted", status.Subtitles)
	}
}

func TestSubtitleTargetsRejectAmbiguousNames(t *testing.T) {
	// Same stem, two video extensions: one subtitle name cannot belong to both.
	data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{
		"movie.mkv": []byte("video"),
		"movie.mp4": []byte("other video"),
	})
	sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))
	addTorrentBytes(t, sess, data)

	status, err := sess.TorrentStatusFor(hash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor: %v", err)
	}
	if len(status.SubtitleTargets) != 2 {
		t.Fatalf("targets = %+v, want both same-stem videos reported", status.SubtitleTargets)
	}
	for _, target := range status.SubtitleTargets {
		if target.Uploadable || target.Reason != session.SubtitleCodeNameConflict {
			t.Fatalf("target = %+v, want both videos marked unuploadable", target)
		}
	}
	_, err = sess.UploadSubtitle(context.Background(), hash.HexString(), "movie.mkv", "movie.srt", strings.NewReader("x"), 1<<20)
	assertSubtitleCode(t, err, session.SubtitleCodeNameConflict)
}

func TestSingleFileSubtitleTargetAndRootCollision(t *testing.T) {
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	sess := newManageSession(t, torrentsDir)

	first, firstHash := buildSingleFileTorrentBytes(t, "Movie.mkv", []byte("first video"), nil)
	addTorrentBytes(t, sess, first)
	second, secondHash := buildSingleFileTorrentBytes(t, "Movie.mkv", []byte("second video"), nil)
	addTorrentBytes(t, sess, second)

	// Exactly one of the two duplicate-named single-file torrents keeps the
	// plain root name; the disambiguated one can no longer match a subtitle
	// name to its visible video.
	uploadable := make([]metainfo.Hash, 0, 2)
	for _, hash := range []metainfo.Hash{firstHash, secondHash} {
		status, err := sess.TorrentStatusFor(hash.HexString())
		if err != nil {
			t.Fatalf("TorrentStatusFor: %v", err)
		}
		if len(status.SubtitleTargets) != 1 {
			t.Fatalf("targets = %+v, want one target for a single-file torrent", status.SubtitleTargets)
		}
		if status.SubtitleTargets[0].Uploadable {
			uploadable = append(uploadable, hash)
		} else if status.SubtitleTargets[0].Reason != session.SubtitleCodeNameConflict {
			t.Fatalf("target = %+v, want a name conflict reason", status.SubtitleTargets[0])
		}
	}
	if len(uploadable) != 1 {
		t.Fatalf("uploadable single-file torrents = %d, want exactly 1", len(uploadable))
	}
	hash := uploadable[0]
	result := uploadSubtitle(t, sess, hash, "Movie.mkv", "Movie.srt", "single\n")
	if result.Path != "Movie.srt" || result.MountPath != "Movie.srt" {
		t.Fatalf("result = %+v, want Movie.srt beside the single-file video", result)
	}
	views := sess.Torrents()
	sawSubtitle := false
	for _, view := range views {
		for _, subtitle := range view.Subtitles {
			if subtitle.Path == "Movie.srt" {
				sawSubtitle = true
			}
		}
	}
	if !sawSubtitle {
		t.Fatal("single-file subtitle is missing from the filesystem view")
	}
}

func TestSubtitleSurvivesRestartAndCleansCrashResidue(t *testing.T) {
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{"Season 1/E01.mp4": []byte("video")})

	sess := newManageSession(t, torrentsDir)
	addTorrentBytes(t, sess, data)
	uploadSubtitle(t, sess, hash, "Season 1/E01.mp4", "E01.srt", "subtitle body\n")
	root := sess.SubtitleRootForTest()
	if err := os.WriteFile(filepath.Join(root, hash.HexString(), ".torrentfs-subtitle-crash.tmp"), []byte("residue"), 0o600); err != nil {
		t.Fatalf("plant crash residue: %v", err)
	}
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := newManageSession(t, torrentsDir)
	status, err := reopened.TorrentStatusFor(hash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor after restart: %v", err)
	}
	if len(status.Subtitles) != 1 || status.Subtitles[0].Path != "Season 1/E01.srt" {
		t.Fatalf("subtitles after restart = %+v, want the nested srt", status.Subtitles)
	}
	if got := readSubtitle(t, reopened, hash, "Season 1/E01.srt"); got != "subtitle body\n" {
		t.Fatalf("subtitle after restart = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, hash.HexString(), ".torrentfs-subtitle-crash.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crash residue survived the startup scan: %v", err)
	}
}

func TestSubtitleUnmanagedSidecarsFailClosed(t *testing.T) {
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{"Movie.2026.mkv": []byte("video")})

	sess := newManageSession(t, torrentsDir)
	addTorrentBytes(t, sess, data)
	uploadSubtitle(t, sess, hash, "Movie.2026.mkv", "Movie.2026.srt", "managed\n")
	root := sess.SubtitleRootForTest()
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := os.Symlink(filepath.Join(root, hash.HexString(), "Movie.2026.srt"), filepath.Join(root, hash.HexString(), "escaped.srt")); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}
	if _, err := session.New(testConfig(), torrentsDir); err == nil {
		t.Fatal("session started with an unmanaged symlink in the subtitle store")
	}
}

func TestSubtitleReadDoesNotFollowAHostPlantedSymlink(t *testing.T) {
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{"Movie.2026.mkv": []byte("video")})

	sess := newManageSession(t, torrentsDir)
	addTorrentBytes(t, sess, data)
	uploadSubtitle(t, sess, hash, "Movie.2026.mkv", "Movie.2026.srt", "managed\n")

	// Replace the torrent's subtitle directory with a symlink to an unrelated
	// tree that holds a same-named file. The read must fail instead of serving
	// the attacker's content, and the mount must not leak it.
	outside := filepath.Join(work, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("make outside dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "Movie.2026.srt"), []byte("forged\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	store := filepath.Join(sess.SubtitleRootForTest(), hash.HexString())
	if err := os.RemoveAll(store); err != nil {
		t.Fatalf("clear store: %v", err)
	}
	if err := os.Symlink(outside, store); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	if _, err := sess.OpenSubtitle(hash, "Movie.2026.srt"); err == nil {
		t.Fatal("OpenSubtitle followed a host-planted symlink out of the subtitle store")
	}
}

func TestDeleteRemovesManagedSubtitles(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{"Season 1/E01.mp4": []byte("video")})

	sess := newManageSession(t, torrentsDir)
	addTorrentBytes(t, sess, data)
	uploadSubtitle(t, sess, hash, "Season 1/E01.mp4", "E01.srt", "srt\n")
	uploadSubtitle(t, sess, hash, "Season 1/E01.mp4", "E01.ass", "ass\n")
	storeDir := filepath.Join(sess.SubtitleRootForTest(), hash.HexString())

	op, err := sess.DeleteTorrent(ctx, hash.HexString())
	if err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}
	if final := waitOperation(t, sess, op.ID); final.State != session.StateDeleted || final.ErrorCode != "" {
		t.Fatalf("delete operation = %+v, want deleted with no error code", final)
	}
	if _, err := os.Stat(storeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("subtitle store survived the delete: %v", err)
	}
	if views := sess.Torrents(); len(views) != 0 {
		t.Fatalf("filesystem views = %+v, want empty", views)
	}
}

func TestPruneRemovesManagedSubtitles(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	// The payload must be a supported video, because only videos can own a
	// managed subtitle.
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "aged.mkv", []byte("aged video"), nil)
	if err := os.WriteFile(filepath.Join(torrentsDir, hash.HexString()+".torrent"), torrentBytes, 0o644); err != nil {
		t.Fatalf("write aged metainfo: %v", err)
	}
	writeFavoriteRegistry(t, torrentsDir, hash, string(session.StateReady), time.Now().UTC().Add(-40*24*time.Hour), false)

	sess := newManageSession(t, torrentsDir)
	uploadSubtitle(t, sess, hash, "aged.mkv", "aged.srt", "aged subtitle\n")
	storeDir := filepath.Join(sess.SubtitleRootForTest(), hash.HexString())

	result, err := sess.DeleteUnfavoritedOlderThan(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("DeleteUnfavoritedOlderThan: %v", err)
	}
	if len(result.Operations) != 1 {
		t.Fatalf("prune started %d deletions, want 1", len(result.Operations))
	}
	if final := waitOperation(t, sess, result.Operations[0].ID); final.State != session.StateDeleted {
		t.Fatalf("prune operation = %+v, want deleted", final)
	}
	if _, err := os.Stat(storeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("subtitle store survived the prune: %v", err)
	}
}

func TestDeleteFailureBlocksUploadAndRetriesWithSameOperation(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{"Movie.2026.mkv": []byte("video")})

	sess := newManageSession(t, torrentsDir)
	addTorrentBytes(t, sess, data)
	uploadSubtitle(t, sess, hash, "Movie.2026.mkv", "Movie.2026.srt", "srt\n")
	storeDir := filepath.Join(sess.SubtitleRootForTest(), hash.HexString())

	restore := session.SetSubtitleCleanupHook(func(metainfo.Hash) error { return errors.New("permission denied") })
	defer restore()

	op, err := sess.DeleteTorrent(ctx, hash.HexString())
	if err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}
	failed := waitOperation(t, sess, op.ID)
	if failed.State != session.StateDeleteFailed {
		t.Fatalf("operation = %+v, want delete_failed", failed)
	}
	if failed.ErrorCode != session.SubtitleCodeCleanupFailed {
		t.Fatalf("error code = %q, want %q", failed.ErrorCode, session.SubtitleCodeCleanupFailed)
	}
	// The failed deletion keeps owning the hash: no payload view, no subtitle
	// view, and no further upload.
	if views := sess.Torrents(); len(views) != 0 {
		t.Fatalf("filesystem views = %+v, want the failed torrent hidden", views)
	}
	if status, err := sess.TorrentStatusFor(hash.HexString()); err != nil {
		t.Fatalf("TorrentStatusFor: %v", err)
	} else if len(status.Subtitles) != 0 || len(status.SubtitleTargets) != 0 {
		t.Fatalf("status = %+v, want no exposed subtitle state", status)
	}
	_, err = sess.UploadSubtitle(context.Background(), hash.HexString(), "Movie.2026.mkv", "Movie.2026.srt", strings.NewReader("late"), 1<<20)
	assertSubtitleCode(t, err, session.SubtitleCodeTorrentDeleting)
	if _, err := os.Stat(storeDir); err != nil {
		t.Fatalf("failed cleanup removed the subtitle store anyway: %v", err)
	}

	// Repairing the fault and deleting again reuses the operation and finishes.
	restore()
	retry, err := sess.DeleteTorrent(ctx, hash.HexString())
	if err != nil {
		t.Fatalf("retry DeleteTorrent: %v", err)
	}
	if retry.ID != failed.ID {
		t.Fatalf("retry operation id = %q, want the original %q", retry.ID, failed.ID)
	}
	if final := waitOperation(t, sess, retry.ID); final.State != session.StateDeleted {
		t.Fatalf("retry operation = %+v, want deleted", final)
	}
	if _, err := os.Stat(storeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("subtitle store survived the retry: %v", err)
	}
}

func TestStartupResumeRetriesDeleteFailedOnce(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{"Movie.2026.mkv": []byte("video")})

	sess := newManageSession(t, torrentsDir)
	addTorrentBytes(t, sess, data)
	uploadSubtitle(t, sess, hash, "Movie.2026.mkv", "Movie.2026.srt", "srt\n")
	storeDir := filepath.Join(sess.SubtitleRootForTest(), hash.HexString())

	restore := session.SetSubtitleCleanupHook(func(metainfo.Hash) error { return errors.New("storage offline") })
	op, err := sess.DeleteTorrent(ctx, hash.HexString())
	if err != nil {
		t.Fatalf("DeleteTorrent: %v", err)
	}
	if failed := waitOperation(t, sess, op.ID); failed.State != session.StateDeleteFailed {
		t.Fatalf("operation = %+v, want delete_failed", failed)
	}
	if err := sess.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The fault is still present: the daemon must start, keep the task hidden,
	// and leave the operation queryable rather than retrying in a loop.
	stillFailing := newManageSession(t, torrentsDir)
	if views := stillFailing.Torrents(); len(views) != 0 {
		t.Fatalf("filesystem views = %+v, want the failed torrent hidden after restart", views)
	}
	if _, err := stillFailing.TorrentViewFor(hash.HexString()); err != nil {
		t.Fatalf("TorrentViewFor after restart: %v", err)
	}
	if op, ok := stillFailing.Operation(op.ID); !ok || op.State != session.StateDeleteFailed || op.ErrorCode != session.SubtitleCodeCleanupFailed {
		t.Fatalf("operation after restart = %+v (%v), want delete_failed with a cleanup code", op, ok)
	}
	if err := stillFailing.Close(ctx); err != nil {
		t.Fatalf("Close failing session: %v", err)
	}

	// Once the fault is repaired, the next start completes the deletion without
	// an operator issuing another DELETE.
	restore()
	recovered := newManageSession(t, torrentsDir)
	if views := recovered.Torrents(); len(views) != 0 {
		t.Fatalf("filesystem views after recovery = %+v, want empty", views)
	}
	if _, err := recovered.TorrentViewFor(hash.HexString()); !errors.Is(err, session.ErrUnknownTorrent) {
		t.Fatalf("TorrentViewFor after recovery = %v, want ErrUnknownTorrent", err)
	}
	if _, err := os.Stat(registryPath(torrentsDir, hash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("registry sidecar survived the recovered deletion: %v", err)
	}
	if _, err := os.Stat(storeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("subtitle store survived the recovered deletion: %v", err)
	}
	if op, ok := recovered.Operation(op.ID); !ok || op.State != session.StateDeleted {
		t.Fatalf("operation after recovery = %+v (%v), want deleted", op, ok)
	}
}

// TestConcurrentAddWaitsForSubtitleUpload pins the concurrency invariant: an
// add must not decide the root layout while a subtitle upload for an existing
// video is mid-flight, because that upload is about to attach itself to the
// layout the add is changing.
func TestConcurrentAddWaitsForSubtitleUpload(t *testing.T) {
	ctx := testTimeout(t)
	torrentsDir := testTorrentDir(t, filepath.Join(t.TempDir(), "data"))
	existingBytes, existingHash := buildSingleFileTorrentBytes(t, "Movie.mkv", []byte("original video"), nil)

	sess := newManageSession(t, torrentsDir)
	addTorrentBytes(t, sess, existingBytes)

	// A same-named single-file torrent whose hash sorts first would take the
	// plain root name away from the video the subtitle belongs to.
	collidingBytes, collidingHash := singleFileTorrentWithNameOrdering(t, "Movie.mkv", existingHash, true)

	// Hold the upload inside its staging write, which is inside the namespace
	// critical section, and let the add race it.
	blocked := make(chan struct{})
	release := make(chan struct{})
	var blockedOnce sync.Once
	restoreFault := session.SetSubtitleIOFault(func(stage string) error {
		if stage == session.SubtitleStageWrite {
			blockedOnce.Do(func() { close(blocked) })
			<-release
		}
		return nil
	})
	defer restoreFault()

	uploadDone := make(chan error, 1)
	go func() {
		_, err := sess.UploadSubtitle(context.Background(), existingHash.HexString(), "Movie.mkv", "Movie.srt", strings.NewReader("managed\n"), 1<<20)
		uploadDone <- err
	}()
	select {
	case <-blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("the upload never reached its staging write")
	}

	addDone := make(chan error, 1)
	go func() {
		_, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: collidingBytes})
		addDone <- err
	}()
	select {
	case err := <-addDone:
		t.Fatalf("add returned %v while a subtitle upload was mid-flight", err)
	case <-time.After(2 * time.Second):
	}

	close(release)
	if err := <-uploadDone; err != nil {
		t.Fatalf("upload: %v", err)
	}
	select {
	case err := <-addDone:
		if !errors.Is(err, session.ErrSubtitleNamespaceConflict) {
			t.Fatalf("add after the upload published = %v, want ErrSubtitleNamespaceConflict", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("add did not finish after the upload published")
	}

	// The published pair survived and the refused add left nothing behind.
	status, err := sess.TorrentStatusFor(existingHash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor: %v", err)
	}
	if len(status.Subtitles) != 1 || status.Subtitles[0].VideoPath != "Movie.mkv" || status.Subtitles[0].MountPath != "Movie.srt" {
		t.Fatalf("subtitles = %+v, want the published pair intact", status.Subtitles)
	}
	if views := sess.Torrents(); len(views) != 1 || views[0].Name != "Movie.mkv" {
		t.Fatalf("filesystem views = %+v, want only the original video at its own name", views)
	}
	if listed := sess.ListTorrents(); len(listed) != 1 {
		t.Fatalf("ListTorrents = %+v, want only the original torrent", listed)
	}
	if _, err := os.Stat(filepath.Join(torrentsDir, collidingHash.HexString()+".torrent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused add left a metainfo sidecar: %v", err)
	}
}

// TestSubtitleUploadRefusesSymlinkedStoreParent pins the write confinement: a
// host process that swaps the torrent's subtitle directory for a symlink into a
// tree the daemon can also write must not be able to redirect the staging file
// or the published subtitle outside the managed store.
func TestSubtitleUploadRefusesSymlinkedStoreParent(t *testing.T) {
	for _, replacing := range []bool{false, true} {
		name := "create"
		if replacing {
			name = "replace"
		}
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
			data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{"Movie.mkv": []byte("video")})

			sess := newManageSession(t, torrentsDir)
			addTorrentBytes(t, sess, data)
			if replacing {
				uploadSubtitle(t, sess, hash, "Movie.mkv", "Movie.srt", "original\n")
			}

			torrentDir := filepath.Join(sess.SubtitleRootForTest(), hash.HexString())
			escape := filepath.Join(work, "escape")
			if err := os.MkdirAll(escape, 0o755); err != nil {
				t.Fatalf("make escape dir: %v", err)
			}
			moved := filepath.Join(work, "moved-store")

			restore := session.SetSubtitleIOFault(func(stage string) error {
				if stage != session.SubtitleStageParents {
					return nil
				}
				// Swap the managed directory for a symlink in the window between
				// the store checks and the write.
				if err := os.Rename(torrentDir, moved); err != nil {
					t.Errorf("move managed directory: %v", err)
					return nil
				}
				if err := os.Symlink(escape, torrentDir); err != nil {
					t.Errorf("plant symlink: %v", err)
				}
				return nil
			})
			defer restore()

			if _, err := sess.UploadSubtitle(context.Background(), hash.HexString(), "Movie.mkv", "Movie.srt", strings.NewReader("escaped\n"), 1<<20); err == nil {
				t.Fatal("upload through a symlinked store parent reported success")
			}
			entries, err := os.ReadDir(escape)
			if err != nil {
				t.Fatalf("read escape dir: %v", err)
			}
			if len(entries) != 0 {
				names := make([]string, 0, len(entries))
				for _, entry := range entries {
					names = append(names, entry.Name())
				}
				t.Fatalf("upload escaped the managed store: %v", names)
			}
			// The moved-aside managed directory still holds its own state.
			movedEntries, err := os.ReadDir(moved)
			if err != nil {
				t.Fatalf("read moved store: %v", err)
			}
			if replacing && len(movedEntries) != 1 {
				t.Fatalf("moved store holds %d entries, want the original subtitle", len(movedEntries))
			}
			// The in-memory index was not advanced for the refused upload.
			if got := readSubtitleCount(t, sess, hash); got != boolToCount(replacing) {
				t.Fatalf("published subtitles = %d, want %d", got, boolToCount(replacing))
			}
			assertNoStagingResidue(t, sess, hash)
		})
	}
}

func boolToCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

// readSubtitleCount reports how many subtitles the session publishes for hash.
func readSubtitleCount(t *testing.T, sess *session.Session, hash metainfo.Hash) int {
	t.Helper()
	status, err := sess.TorrentStatusFor(hash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor: %v", err)
	}
	return len(status.Subtitles)
}

// TestSubtitleUploadRefusesSwappedStoreRoot pins the persistent handle: once the
// session is running, moving the store away and putting a symlink in its place
// must neither receive writes nor redirect reads. The session keeps using the
// directory it opened and refuses the change.
func TestSubtitleUploadRefusesSwappedStoreRoot(t *testing.T) {
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{"Movie.mkv": []byte("video")})

	sess := newManageSession(t, torrentsDir)
	addTorrentBytes(t, sess, data)
	uploadSubtitle(t, sess, hash, "Movie.mkv", "Movie.srt", "original\n")

	store := sess.SubtitleRootForTest()
	moved := filepath.Join(work, "moved-store")
	if err := os.Rename(store, moved); err != nil {
		t.Fatalf("move store: %v", err)
	}
	escape := filepath.Join(work, "escape")
	if err := os.MkdirAll(escape, 0o755); err != nil {
		t.Fatalf("make escape dir: %v", err)
	}
	if err := os.Symlink(escape, store); err != nil {
		t.Fatalf("plant store symlink: %v", err)
	}

	_, err := sess.UploadSubtitle(context.Background(), hash.HexString(), "Movie.mkv", "Movie.srt", strings.NewReader("after swap\n"), 1<<20)
	assertSubtitleCode(t, err, session.SubtitleCodeStorageUnavailable)
	entries, err := os.ReadDir(escape)
	if err != nil {
		t.Fatalf("read escape dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("upload escaped into the replacement directory: %v", names)
	}
	// Reads keep working from the directory the session opened.
	if got := readSubtitle(t, sess, hash, "Movie.srt"); got != "original\n" {
		t.Fatalf("subtitle read after the swap = %q, want the published content", got)
	}
}

// TestSubtitleUploadRefusesCrossHashSymlink pins the no-follow rule: replacing
// one torrent's store directory with a relative symlink to another torrent's
// directory must not let the first torrent write into or read the second one's
// tree.
func TestSubtitleUploadRefusesCrossHashSymlink(t *testing.T) {
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	aBytes, aHash := buildSingleFileTorrentBytes(t, "A.mkv", []byte("a video"), nil)
	bBytes, bHash := buildSingleFileTorrentBytes(t, "B.mkv", []byte("b video"), nil)

	sess := newManageSession(t, torrentsDir)
	addTorrentBytes(t, sess, aBytes)
	addTorrentBytes(t, sess, bBytes)
	uploadSubtitle(t, sess, bHash, "B.mkv", "B.srt", "b subtitle\n")

	store := sess.SubtitleRootForTest()
	// A relative link that stays inside the store: a path-following
	// implementation would resolve it and write A's subtitle into B's tree.
	if err := os.Symlink(bHash.HexString(), filepath.Join(store, aHash.HexString())); err != nil {
		t.Fatalf("plant cross-hash symlink: %v", err)
	}

	_, err := sess.UploadSubtitle(context.Background(), aHash.HexString(), "A.mkv", "A.srt", strings.NewReader("a subtitle\n"), 1<<20)
	assertSubtitleCode(t, err, session.SubtitleCodeStorageUnavailable)
	if status, err := sess.TorrentStatusFor(aHash.HexString()); err != nil {
		t.Fatalf("TorrentStatusFor A: %v", err)
	} else if len(status.Subtitles) != 0 {
		t.Fatalf("torrent A published %+v, want nothing through a redirected path", status.Subtitles)
	}
	// B's tree only holds B's own subtitle, and it still reads back.
	if got := readSubtitle(t, sess, bHash, "B.srt"); got != "b subtitle\n" {
		t.Fatalf("B subtitle = %q, want it untouched", got)
	}
	entries, err := os.ReadDir(filepath.Join(store, bHash.HexString()))
	if err != nil {
		t.Fatalf("read B store dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 1 || names[0] != "B.srt" {
		t.Fatalf("B store dir holds %v, want only B.srt", names)
	}
}

// TestSubtitleOpenMetadataMatchesTheOpenedFile pins the per-open snapshot: a
// replacement that lands between a subtitle's open and its metadata read must
// not pair one version's length with another version's bytes, in either
// direction.
func TestSubtitleOpenMetadataMatchesTheOpenedFile(t *testing.T) {
	long := strings.Repeat("long-subtitle\n", 256)
	for _, tt := range []struct {
		name        string
		initial     string
		replacement string
	}{
		{"short to long", "short\n", long},
		{"long to short", long, "short\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			work := t.TempDir()
			torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
			data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{"Movie.mkv": []byte("video")})

			sess := newManageSession(t, torrentsDir)
			addTorrentBytes(t, sess, data)
			uploadSubtitle(t, sess, hash, "Movie.mkv", "Movie.srt", tt.initial)

			// Land a replacement inside the open, after the descriptor exists.
			swapped := false
			store := sess.SubtitleRootForTest()
			restore := session.SetSubtitleOpenHook(func(hash metainfo.Hash, path string) {
				if swapped {
					return
				}
				swapped = true
				dir := filepath.Join(store, hash.HexString())
				replacement := filepath.Join(dir, "inside-open.tmp")
				if err := os.WriteFile(replacement, []byte(tt.replacement), 0o644); err != nil {
					t.Errorf("write replacement: %v", err)
					return
				}
				if err := os.Rename(replacement, filepath.Join(dir, path)); err != nil {
					t.Errorf("publish replacement: %v", err)
				}
			})
			defer restore()

			snapshot, err := sess.OpenSubtitle(hash, "Movie.srt")
			if err != nil {
				t.Fatalf("OpenSubtitle: %v", err)
			}
			defer func() {
				if closer, ok := snapshot.Reader.(io.Closer); ok {
					_ = closer.Close()
				}
			}()
			if !swapped {
				t.Fatal("the open hook never ran")
			}
			// The length and the bytes describe the version the open holds.
			if snapshot.Size != int64(len(tt.initial)) {
				t.Fatalf("opened size = %d, want the opened version's %d", snapshot.Size, len(tt.initial))
			}
			opened, err := io.ReadAll(io.NewSectionReader(snapshot.Reader, 0, snapshot.Size))
			if err != nil {
				t.Fatalf("read the opened length: %v", err)
			}
			if string(opened) != tt.initial {
				t.Fatalf("opened content = %q, want the opened version", opened)
			}
		})
	}
}

// TestMagnetMetadataPublishRefusedBeforeWriting pins the metadata order: a
// resolved magnet is refused before its final metainfo is written, so a crash
// cannot leave a file that a restart would accept.
func TestMagnetMetadataPublishRefusedBeforeWriting(t *testing.T) {
	ctx := testTimeout(t)
	torrentsDir := testTorrentDir(t, filepath.Join(t.TempDir(), "data"))
	existingBytes, existingHash := buildSingleFileTorrentBytes(t, "Movie.mkv", []byte("original video"), nil)

	sess := newManageSession(t, torrentsDir)
	addTorrentBytes(t, sess, existingBytes)
	uploadSubtitle(t, sess, existingHash, "Movie.mkv", "Movie.srt", "managed\n")

	collidingBytes, collidingHash := singleFileTorrentWithNameOrdering(t, "Movie.mkv", existingHash, true)
	magnet := "magnet:?xt=urn:btih:" + collidingHash.HexString() + "&dn=Movie.mkv"
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{MagnetURI: magnet}); err != nil {
		t.Fatalf("magnet add: %v", err)
	}
	st, ok := sess.Torrent(collidingHash)
	if !ok {
		t.Fatal("magnet torrent was not registered")
	}

	// The magnet add already started the persist worker. Delivering the metadata
	// the way a peer would lets that same worker run; the file must not appear
	// under any circumstance.
	if err := session.DeliverMetadataForTest(st, collidingBytes); err != nil {
		t.Fatalf("deliver metadata: %v", err)
	}
	if err := waitFor(ctx, func() bool {
		view, err := sess.TorrentViewFor(collidingHash.HexString())
		return err == nil && view.State != session.StateAdding
	}); err != nil {
		t.Fatalf("metadata persist never settled: %v", err)
	}

	finalPath := filepath.Join(torrentsDir, collidingHash.HexString()+".torrent")
	if _, err := os.Stat(finalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused magnet publish wrote %s", finalPath)
	}
	// The conflict is recorded, not swallowed, and the torrent stays hidden.
	view, err := sess.TorrentViewFor(collidingHash.HexString())
	if err != nil {
		t.Fatalf("TorrentViewFor: %v", err)
	}
	if view.State != session.StateError || !strings.Contains(view.Error, "namespace conflict") {
		t.Fatalf("refused magnet view = %+v, want an error naming the conflict", view)
	}
	if views := sess.Torrents(); len(views) != 1 || views[0].Name != "Movie.mkv" {
		t.Fatalf("filesystem views = %+v, want only the original video", views)
	}
	status, err := sess.TorrentStatusFor(existingHash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor: %v", err)
	}
	if len(status.Subtitles) != 1 || status.Subtitles[0].MountPath != "Movie.srt" {
		t.Fatalf("subtitles = %+v, want the pair untouched", status.Subtitles)
	}
}

// TestRestartRefusesPersistedLayoutThatBreaksSubtitle pins the startup check: a
// final metainfo left by a crash, with a registry that is not ready, must not be
// resurrected into a layout that renames a subtitled video.
func TestRestartRefusesPersistedLayoutThatBreaksSubtitle(t *testing.T) {
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	existingBytes, existingHash := buildSingleFileTorrentBytes(t, "Movie.mkv", []byte("original video"), nil)

	sess := newManageSession(t, torrentsDir)
	addTorrentBytes(t, sess, existingBytes)
	uploadSubtitle(t, sess, existingHash, "Movie.mkv", "Movie.srt", "managed\n")
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	collidingBytes, collidingHash := singleFileTorrentWithNameOrdering(t, "Movie.mkv", existingHash, true)
	if err := os.WriteFile(filepath.Join(torrentsDir, collidingHash.HexString()+".torrent"), collidingBytes, 0o644); err != nil {
		t.Fatalf("write stranded metainfo: %v", err)
	}
	writeRegistry(t, torrentsDir, collidingHash, string(session.StateAdding), "op-stranded")

	if _, err := session.New(testConfig(), torrentsDir); !errors.Is(err, session.ErrSubtitleNamespaceConflict) {
		t.Fatalf("restart = %v, want ErrSubtitleNamespaceConflict", err)
	}

	// Removing the stranded task restores a startable state, proving the refusal
	// is about the persisted layout and not the fixture. The refusal above
	// normalized the registry entry to ready, so its state sidecar goes too.
	if err := os.Remove(filepath.Join(torrentsDir, collidingHash.HexString()+".torrent")); err != nil {
		t.Fatalf("remove stranded metainfo: %v", err)
	}
	if err := os.Remove(registryPath(torrentsDir, collidingHash)); err != nil {
		t.Fatalf("remove stranded registry entry: %v", err)
	}
	recovered := newManageSession(t, torrentsDir)
	status, err := recovered.TorrentStatusFor(existingHash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor after recovery: %v", err)
	}
	if len(status.Subtitles) != 1 || status.Subtitles[0].VideoPath != "Movie.mkv" {
		t.Fatalf("subtitles after recovery = %+v, want the pair intact", status.Subtitles)
	}
}

// TestRestartRefusesPersistedTorrentThatShadowsSubtitle covers the layout rule
// the per-subtitle scan cannot see: a restored torrent whose root name is
// exactly a managed subtitle's path would hide that subtitle from the mount,
// even though the subtitle still resolves to its own video.
func TestRestartRefusesPersistedTorrentThatShadowsSubtitle(t *testing.T) {
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	existingBytes, existingHash := buildSingleFileTorrentBytes(t, "Movie.mkv", []byte("original video"), nil)

	sess := newManageSession(t, torrentsDir)
	addTorrentBytes(t, sess, existingBytes)
	uploadSubtitle(t, sess, existingHash, "Movie.mkv", "Movie.srt", "managed\n")
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	shadowBytes, shadowHash := buildSingleFileTorrentBytes(t, "Movie.srt", []byte("shadowing file"), nil)
	if err := os.WriteFile(filepath.Join(torrentsDir, shadowHash.HexString()+".torrent"), shadowBytes, 0o644); err != nil {
		t.Fatalf("write shadowing metainfo: %v", err)
	}
	writeRegistry(t, torrentsDir, shadowHash, string(session.StateReady), "op-shadow")

	if _, err := session.New(testConfig(), torrentsDir); !errors.Is(err, session.ErrSubtitleNamespaceConflict) {
		t.Fatalf("restart with a shadowing root name = %v, want ErrSubtitleNamespaceConflict", err)
	}
}

// TestPostRenameFailureKeepsEveryViewConsistent pins the commit boundary: once
// the publish rename has happened the upload has succeeded, so a later
// durability failure must not report a failed write for content that is live.
func TestPostRenameFailureKeepsEveryViewConsistent(t *testing.T) {
	handler, logState := newPhaseLogHandler()
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{"Movie.mkv": []byte("video")})

	sess, err := session.New(testConfig(), torrentsDir, session.WithLogger(slog.New(handler)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	addTorrentBytes(t, sess, data)
	uploadSubtitle(t, sess, hash, "Movie.mkv", "Movie.srt", "old content\n")

	restore := session.SetSubtitleIOFault(func(stage string) error {
		if stage == session.SubtitleStageCommit {
			return errors.New("directory sync failed")
		}
		return nil
	})
	defer restore()

	replaced := uploadSubtitle(t, sess, hash, "Movie.mkv", "Movie.srt", "new content\n")
	if !replaced.Replaced || replaced.Size != int64(len("new content\n")) {
		t.Fatalf("replacement = %+v, want the committed new content", replaced)
	}
	if got := readSubtitle(t, sess, hash, "Movie.srt"); got != "new content\n" {
		t.Fatalf("subtitle after the post-rename failure = %q, want the new content", got)
	}
	assertNoStagingResidue(t, sess, hash)

	// A create after the rename commits too, and is visible everywhere.
	created := uploadSubtitle(t, sess, hash, "Movie.mkv", "Movie.ass", "ass content\n")
	if created.Replaced {
		t.Fatal("a new extension reported a replacement")
	}
	if got := readSubtitle(t, sess, hash, "Movie.ass"); got != "ass content\n" {
		t.Fatalf("created subtitle = %q, want the committed content", got)
	}
	status, err := sess.TorrentStatusFor(hash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor: %v", err)
	}
	sizes := map[string]int64{}
	for _, subtitle := range status.Subtitles {
		sizes[subtitle.Path] = subtitle.Size
	}
	if sizes["Movie.srt"] != int64(len("new content\n")) || sizes["Movie.ass"] != int64(len("ass content\n")) {
		t.Fatalf("published sizes = %+v, want both committed subtitles listed", sizes)
	}
	if !logState.messagesContaining("subtitle durability step failed after publish") {
		t.Fatalf("the durability failure was not surfaced: %v", logState.messages())
	}
}

// singleFileTorrentWithNameOrdering finds a single-file torrent named name
// whose info hash sorts below or above bound. Root names are assigned in hash
// order, so this is how a test decides which torrent keeps the plain name.
func singleFileTorrentWithNameOrdering(t *testing.T, name string, bound metainfo.Hash, lower bool) ([]byte, metainfo.Hash) {
	t.Helper()
	for i := 0; i < 2000; i++ {
		torrentBytes, hash := buildSingleFileTorrentBytes(t, name, []byte(fmt.Sprintf("%s payload %d", name, i)), nil)
		if (hash.HexString() < bound.HexString()) == lower {
			return torrentBytes, hash
		}
	}
	t.Fatalf("no single-file torrent named %q found with lower=%v than %s", name, lower, bound)
	return nil, metainfo.Hash{}
}

// TestAddRejectedWhenItWouldRenameSubtitleVideo pins the namespace guard: a new
// same-named single-file torrent that would take the plain root name from an
// already-subtitled video is refused before anything is published, instead of
// silently renaming the video away from its subtitle.
func TestAddRejectedWhenItWouldRenameSubtitleVideo(t *testing.T) {
	ctx := testTimeout(t)
	torrentsDir := testTorrentDir(t, filepath.Join(t.TempDir(), "data"))
	existingBytes, existingHash := buildSingleFileTorrentBytes(t, "Movie.mkv", []byte("original video"), nil)

	sess := newManageSession(t, torrentsDir)
	addTorrentBytes(t, sess, existingBytes)
	uploadSubtitle(t, sess, existingHash, "Movie.mkv", "Movie.srt", "managed\n")

	collidingBytes, collidingHash := singleFileTorrentWithNameOrdering(t, "Movie.mkv", existingHash, true)
	_, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: collidingBytes})
	if !errors.Is(err, session.ErrSubtitleNamespaceConflict) {
		t.Fatalf("colliding add = %v, want ErrSubtitleNamespaceConflict", err)
	}
	// The rejected add published nothing.
	if _, err := os.Stat(filepath.Join(torrentsDir, collidingHash.HexString()+".torrent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected add left a metainfo sidecar: %v", err)
	}
	if listed := sess.ListTorrents(); len(listed) != 1 || listed[0].InfoHash != existingHash.HexString() {
		t.Fatalf("ListTorrents after a rejected add = %+v, want only the existing torrent", listed)
	}
	status, err := sess.TorrentStatusFor(existingHash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor: %v", err)
	}
	if len(status.Subtitles) != 1 || status.Subtitles[0].VideoPath != "Movie.mkv" || status.Subtitles[0].MountPath != "Movie.srt" {
		t.Fatalf("subtitles after a rejected add = %+v, want the video/subtitle pair intact", status.Subtitles)
	}

	// A same-named torrent that only receives the hash-suffixed root name does
	// not disturb the existing pair, so it is allowed.
	suffixedBytes, suffixedHash := singleFileTorrentWithNameOrdering(t, "Movie.mkv", existingHash, false)
	addTorrentBytes(t, sess, suffixedBytes)
	status, err = sess.TorrentStatusFor(existingHash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor after the allowed add: %v", err)
	}
	if len(status.Subtitles) != 1 || status.Subtitles[0].MountPath != "Movie.srt" {
		t.Fatalf("subtitles after the allowed add = %+v, want the existing mount path", status.Subtitles)
	}
	suffixedTarget := subtitleTarget(t, sess, suffixedHash, "Movie.mkv")
	if suffixedTarget.Uploadable || suffixedTarget.Reason != session.SubtitleCodeNameConflict {
		t.Fatalf("suffixed torrent target = %+v, want its own target blocked", suffixedTarget)
	}
	if _, err := sess.UploadSubtitle(context.Background(), collidingHash.HexString(), "Movie.mkv", "Movie.srt", strings.NewReader("x"), 1<<20); !errors.Is(err, session.ErrUnknownTorrent) {
		t.Fatalf("upload to a rejected torrent = %v, want ErrUnknownTorrent", err)
	}
}

// TestAddRejectedWhenRootNameShadowsSubtitle covers the other half of the guard:
// a new torrent whose root name is exactly an existing subtitle's path would
// hide that subtitle from the mount root.
func TestAddRejectedWhenRootNameShadowsSubtitle(t *testing.T) {
	ctx := testTimeout(t)
	torrentsDir := testTorrentDir(t, filepath.Join(t.TempDir(), "data"))
	existingBytes, existingHash := buildSingleFileTorrentBytes(t, "Movie.mkv", []byte("original video"), nil)

	sess := newManageSession(t, torrentsDir)
	addTorrentBytes(t, sess, existingBytes)
	uploadSubtitle(t, sess, existingHash, "Movie.mkv", "Movie.srt", "managed\n")

	// Above the existing hash, so the older video keeps the plain root name and
	// only the shadowing rule can reject this add.
	shadowBytes, shadowHash := singleFileTorrentWithNameOrdering(t, "Movie.srt", existingHash, false)
	_, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: shadowBytes})
	if !errors.Is(err, session.ErrSubtitleNamespaceConflict) {
		t.Fatalf("shadowing add = %v, want ErrSubtitleNamespaceConflict", err)
	}
	if listed := sess.ListTorrents(); len(listed) != 1 {
		t.Fatalf("ListTorrents after a rejected add = %+v, want only the existing torrent", listed)
	}
	status, err := sess.TorrentStatusFor(existingHash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor: %v", err)
	}
	if len(status.Subtitles) != 1 || status.Subtitles[0].MountPath != "Movie.srt" {
		t.Fatalf("subtitles after a rejected shadowing add = %+v, want the visible subtitle", status.Subtitles)
	}
	if views := sess.Torrents(); len(views) != 1 {
		t.Fatalf("filesystem views = %d, want the shadowing torrent not exposed", len(views))
	}
	_ = shadowHash
}

// TestUnrelatedAddStillAllowed guards against an over-eager namespace check: a
// torrent that shares no root name with an existing subtitle must still add.
func TestUnrelatedAddStillAllowed(t *testing.T) {
	torrentsDir := testTorrentDir(t, filepath.Join(t.TempDir(), "data"))
	existingBytes, existingHash := buildSingleFileTorrentBytes(t, "Movie.mkv", []byte("original video"), nil)
	otherBytes, _ := buildSingleFileTorrentBytes(t, "Other.mkv", []byte("other video"), nil)

	sess := newManageSession(t, torrentsDir)
	addTorrentBytes(t, sess, existingBytes)
	uploadSubtitle(t, sess, existingHash, "Movie.mkv", "Movie.srt", "managed\n")
	addTorrentBytes(t, sess, otherBytes)

	if listed := sess.ListTorrents(); len(listed) != 2 {
		t.Fatalf("ListTorrents = %+v, want both torrents", listed)
	}
	status, err := sess.TorrentStatusFor(existingHash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor: %v", err)
	}
	if len(status.Subtitles) != 1 || status.Subtitles[0].MountPath != "Movie.srt" {
		t.Fatalf("subtitles = %+v, want the existing pair untouched by an unrelated add", status.Subtitles)
	}
}

// TestSubtitleRestartRecoversWithSiblingVideos pins the recovery rule: a
// subtitle belongs to the video whose expected basename it carries, even when
// its directory holds other videos. Matching only by directory used to make
// every sibling a candidate, so the whole session failed to start.
func TestSubtitleRestartRecoversWithSiblingVideos(t *testing.T) {
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{
		"Season/E01.mkv": []byte("first episode"),
		"Season/E02.mkv": []byte("second episode"),
	})

	sess := newManageSession(t, torrentsDir)
	addTorrentBytes(t, sess, data)
	uploadSubtitle(t, sess, hash, "Season/E01.mkv", "E01.srt", "first subtitle\n")
	uploadSubtitle(t, sess, hash, "Season/E02.mkv", "E02.vtt", "WEBVTT second\n")
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := newManageSession(t, torrentsDir)
	status, err := reopened.TorrentStatusFor(hash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor after restart: %v", err)
	}
	if len(status.Subtitles) != 2 {
		t.Fatalf("subtitles after restart = %+v, want both sidecars", status.Subtitles)
	}
	owners := map[string]string{}
	for _, subtitle := range status.Subtitles {
		owners[subtitle.Path] = subtitle.VideoPath
	}
	if owners["Season/E01.srt"] != "Season/E01.mkv" {
		t.Fatalf("Season/E01.srt owner = %q, want Season/E01.mkv", owners["Season/E01.srt"])
	}
	if owners["Season/E02.vtt"] != "Season/E02.mkv" {
		t.Fatalf("Season/E02.vtt owner = %q, want Season/E02.mkv", owners["Season/E02.vtt"])
	}
	if got := readSubtitle(t, reopened, hash, "Season/E01.srt"); got != "first subtitle\n" {
		t.Fatalf("recovered subtitle = %q", got)
	}
}

// TestSubtitleRestartRejectsUnmanagedSidecar pins the tampering rule: a regular
// file the daemon never published is not adopted, and the store does not come
// up with an entry that has no matching video.
func TestSubtitleRestartRejectsUnmanagedSidecar(t *testing.T) {
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{"Season/E01.mkv": []byte("episode")})

	sess := newManageSession(t, torrentsDir)
	addTorrentBytes(t, sess, data)
	uploadSubtitle(t, sess, hash, "Season/E01.mkv", "E01.srt", "managed\n")
	store := sess.SubtitleRootForTest()
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A sibling video appears only on disk, not in the torrent, and a planted
	// sidecar carries a basename no video owns.
	planted := filepath.Join(store, hash.HexString(), "Season", "Other.srt")
	if err := os.WriteFile(planted, []byte("planted\n"), 0o600); err != nil {
		t.Fatalf("plant sidecar: %v", err)
	}
	if _, err := session.New(testConfig(), torrentsDir); err == nil {
		t.Fatal("session started with a planted sidecar that matches no video basename")
	}
	if err := os.Remove(planted); err != nil {
		t.Fatalf("remove planted sidecar: %v", err)
	}
	if _, err := session.New(testConfig(), torrentsDir); err != nil {
		t.Fatalf("session did not start once the planted sidecar was removed: %v", err)
	}
}

// TestVideoExtensionCaseInsensitive covers the allowed video extensions in any
// case: the allowlist is case-insensitive, while the subtitle must still carry
// the video's stem as spelled, because the extension is cut by its own length.
func TestVideoExtensionCaseInsensitive(t *testing.T) {
	data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{
		"Movie.MKV": []byte("upper video"),
		"Clip.Mp4":  []byte("mixed video"),
		"notes.txt": []byte("not a video"),
	})
	sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))
	addTorrentBytes(t, sess, data)

	status, err := sess.TorrentStatusFor(hash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor: %v", err)
	}
	if len(status.SubtitleTargets) != 2 {
		t.Fatalf("targets = %+v, want one per case-insensitive video extension", status.SubtitleTargets)
	}
	expected := map[string]string{"Movie.MKV": "Movie", "Clip.Mp4": "Clip"}
	for _, target := range status.SubtitleTargets {
		if want := expected[target.VideoPath]; target.ExpectedBasename != want {
			t.Fatalf("target %s expected basename = %q, want %q", target.VideoPath, target.ExpectedBasename, want)
		}
		if !target.Uploadable {
			t.Fatalf("target %+v, want uploadable", target)
		}
	}

	// The stem is the name without its extension, in the file's own case.
	uploadSubtitle(t, sess, hash, "Movie.MKV", "Movie.srt", "upper\n")
	uploadSubtitle(t, sess, hash, "Clip.Mp4", "Clip.ass", "mixed\n")
	_, err = sess.UploadSubtitle(context.Background(), hash.HexString(), "Movie.MKV", "Movie.MKV.srt", strings.NewReader("x"), 1<<20)
	assertSubtitleCode(t, err, session.SubtitleCodeNameMismatch)
	_, err = sess.UploadSubtitle(context.Background(), hash.HexString(), "Clip.Mp4", "Clip.Mp4.srt", strings.NewReader("x"), 1<<20)
	assertSubtitleCode(t, err, session.SubtitleCodeNameMismatch)

	// A single-file torrent behaves the same way: the mount path is the stem
	// plus the subtitle extension.
	soloBytes, soloHash := buildSingleFileTorrentBytes(t, "Solo.MKV", []byte("solo video"), nil)
	addTorrentBytes(t, sess, soloBytes)
	soloTarget := subtitleTarget(t, sess, soloHash, "Solo.MKV")
	if soloTarget.ExpectedBasename != "Solo" || soloTarget.MountPath != "Solo.srt" {
		t.Fatalf("single-file target = %+v, want stem Solo and mount path Solo.srt", soloTarget)
	}
	uploadSubtitle(t, sess, soloHash, "Solo.MKV", "Solo.srt", "solo\n")
}

// TestSubtitleUploadStorageErrorsPerStage pins the error contract per I/O
// stage: a full store is reported as one, an unusable store as another, and the
// failing syscall stays reachable for diagnosis. The previous state must
// survive every failure untouched.
func TestSubtitleUploadStorageErrorsPerStage(t *testing.T) {
	stages := []string{
		session.SubtitleStageCreate,
		session.SubtitleStageWrite,
		session.SubtitleStageSync,
		session.SubtitleStageRename,
	}
	errnos := []struct {
		name string
		err  error
		code string
	}{
		{"ENOSPC", unix.ENOSPC, session.SubtitleCodeStorageFull},
		{"EDQUOT", unix.EDQUOT, session.SubtitleCodeStorageFull},
		{"EROFS", unix.EROFS, session.SubtitleCodeStorageUnavailable},
		{"EACCES", unix.EACCES, session.SubtitleCodeStorageUnavailable},
		{"generic", errors.New("device exploded"), session.SubtitleCodeWriteFailed},
	}
	for _, stage := range stages {
		for _, tt := range errnos {
			t.Run(stage+"/"+tt.name, func(t *testing.T) {
				data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{"Movie.mkv": []byte("video")})
				sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))
				addTorrentBytes(t, sess, data)
				uploadSubtitle(t, sess, hash, "Movie.mkv", "Movie.srt", "keep me\n")

				restore := session.SetSubtitleIOFault(func(got string) error {
					if got != stage {
						return nil
					}
					return fmt.Errorf("injected %s: %w", tt.name, tt.err)
				})
				defer restore()

				_, err := sess.UploadSubtitle(context.Background(), hash.HexString(), "Movie.mkv", "Movie.srt", strings.NewReader("failure\n"), 1<<20)
				assertSubtitleCode(t, err, tt.code)
				if !errors.Is(err, tt.err) {
					t.Fatalf("error %v does not wrap the failing cause %v", err, tt.err)
				}
				// The existing subtitle and the staging area are untouched.
				if got := readSubtitle(t, sess, hash, "Movie.srt"); got != "keep me\n" {
					t.Fatalf("subtitle after a failed upload = %q, want the previous content", got)
				}
				assertNoStagingResidue(t, sess, hash)
			})
		}
	}
}

// assertNoStagingResidue fails when a failed upload left an internal temp file.
func assertNoStagingResidue(t *testing.T, sess *session.Session, hash metainfo.Hash) {
	t.Helper()
	root := filepath.Join(sess.SubtitleRootForTest(), hash.HexString())
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".torrentfs-subtitle-") {
				t.Fatalf("staging residue left behind: %s", filepath.Join(dir, entry.Name()))
			}
			if entry.IsDir() {
				walk(filepath.Join(dir, entry.Name()))
			}
		}
	}
	walk(root)
}

func TestSubtitleStoreRejectsTamperedDestination(t *testing.T) {
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{"Movie.2026.mkv": []byte("video")})

	sess := newManageSession(t, torrentsDir)
	addTorrentBytes(t, sess, data)
	uploadSubtitle(t, sess, hash, "Movie.2026.mkv", "Movie.2026.srt", "keep me\n")

	// A directory where the sidecar belongs is not a managed subtitle, so the
	// upload must fail rather than write into it.
	destination := filepath.Join(sess.SubtitleRootForTest(), hash.HexString(), "Movie.2026.vtt")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatalf("plant destination directory: %v", err)
	}
	_, err := sess.UploadSubtitle(context.Background(), hash.HexString(), "Movie.2026.mkv", "Movie.2026.vtt", strings.NewReader("x"), 1<<20)
	assertSubtitleCode(t, err, session.SubtitleCodeStorageUnavailable)
	if got := readSubtitle(t, sess, hash, "Movie.2026.srt"); got != "keep me\n" {
		t.Fatalf("existing subtitle = %q, want it untouched by the failed upload", got)
	}
	entries, err := os.ReadDir(filepath.Join(sess.SubtitleRootForTest(), hash.HexString()))
	if err != nil {
		t.Fatalf("read subtitle store: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".torrentfs-subtitle-") {
			t.Fatalf("failed upload left a staging file behind: %s", entry.Name())
		}
	}
}

func TestSubtitleStoreIsOutsideTheMountRoot(t *testing.T) {
	// The overlay must never be reachable as a payload path, so the store lives
	// under the metadata directory, not in the torrents directory itself.
	work := t.TempDir()
	torrentsDir := testTorrentDir(t, filepath.Join(work, "data"))
	data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{"Movie.2026.mkv": []byte("video")})

	sess := newManageSession(t, torrentsDir)
	addTorrentBytes(t, sess, data)
	uploadSubtitle(t, sess, hash, "Movie.2026.mkv", "Movie.2026.srt", "srt\n")

	root := sess.SubtitleRootForTest()
	if want := filepath.Join(torrentsDir, ".metadata", "subtitles"); root != want {
		t.Fatalf("subtitle root = %q, want %q", root, want)
	}
	// Nothing named like a torrent payload appears in the torrents directory.
	entries, err := os.ReadDir(torrentsDir)
	if err != nil {
		t.Fatalf("read torrents dir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() == hash.HexString() {
			t.Fatalf("subtitle store leaked into the torrents directory: %s", entry.Name())
		}
	}
}

// TestSubtitleRegistryErrorsStayCoded pins the wire contract: each rejection
// carries a stable code, so the web client never branches on display text.
func TestSubtitleRegistryErrorsStayCoded(t *testing.T) {
	data, _ := multiFileTorrentBytes(t, "Show", map[string][]byte{"Movie.2026.mkv": []byte("video")})
	sess := newManageSession(t, testTorrentDir(t, filepath.Join(t.TempDir(), "data")))
	addTorrentBytes(t, sess, data)

	_, err := sess.UploadSubtitle(context.Background(), strings.Repeat("f", 40), "Movie.2026.mkv", "Movie.2026.srt", strings.NewReader("x"), 1<<20)
	if !errors.Is(err, session.ErrUnknownTorrent) {
		t.Fatalf("unknown torrent upload = %v, want ErrUnknownTorrent", err)
	}
	_, err = sess.UploadSubtitle(context.Background(), "not-a-hash", "Movie.2026.mkv", "Movie.2026.srt", strings.NewReader("x"), 1<<20)
	if !errors.Is(err, session.ErrUnknownTorrent) {
		t.Fatalf("malformed id upload = %v, want ErrUnknownTorrent", err)
	}
}
