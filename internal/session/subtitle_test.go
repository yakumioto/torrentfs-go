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

	"github.com/anacrolix/torrent/metainfo"

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
	reader, err := sess.OpenSubtitle(hash, relPath)
	if err != nil {
		t.Fatalf("OpenSubtitle %q: %v", relPath, err)
	}
	if closer, ok := reader.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}
	data, err := io.ReadAll(io.NewSectionReader(reader, 0, 1<<20))
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
