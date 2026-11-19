package session_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

// The deletion path owns a torrent's namespace: while it is running, a subtitle
// must not disappear underneath it. This drives the real session state rather
// than a backend that fabricates the error.
func TestSubtitleDeleteRejectedWhileDeletionHoldsTorrent(t *testing.T) {
	ctx := testTimeout(t)
	torrentDir := testTorrentDir(t, filepath.Join(t.TempDir(), "data"))
	data, hash := multiFileTorrentBytes(t, "Show", map[string][]byte{"A.mkv": []byte("video a")})

	metadataDir := filepath.Join(torrentDir, ".metadata")
	subtitleDir := filepath.Join(metadataDir, "subtitles", hash.HexString())
	if err := os.MkdirAll(subtitleDir, 0o755); err != nil {
		t.Fatalf("make subtitle dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, hash.HexString()+".torrent"), data, 0o644); err != nil {
		t.Fatalf("write metainfo: %v", err)
	}
	subtitlePath := filepath.Join(subtitleDir, "A.srt")
	if err := os.WriteFile(subtitlePath, []byte("a srt\n"), 0o644); err != nil {
		t.Fatalf("write subtitle: %v", err)
	}
	writeRegistry(t, torrentDir, hash, string(session.StateDeleting), "op-subtitle-delete")
	// A failing cleanup keeps the entry alive across the reload, which is how a
	// crash mid-deletion looks to the next process.
	restore := session.SetSubtitleCleanupHook(func(metainfo.Hash) error { return errors.New("cleanup still failing") })
	defer restore()

	sess := newManageSession(t, torrentDir)
	if err := sess.DeleteSubtitle(ctx, hash.HexString(), "A.srt"); !errors.Is(err, session.ErrDeleting) {
		t.Fatalf("DeleteSubtitle while deletion holds the torrent = %v, want ErrDeleting", err)
	}
	if _, err := os.Stat(subtitlePath); err != nil {
		t.Fatalf("subtitle removed while deletion holds the torrent: %v", err)
	}
}
