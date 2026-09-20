package session_test

import (
	"context"
	"crypto/sha1"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

const testPieceLength = 256 << 10

func testConfig() config.Config {
	return config.Default()
}

func testTorrentDir(t *testing.T, base string) string {
	t.Helper()
	dir := filepath.Join(base, "torrents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	return dir
}

// seedPieces fills a session's in-memory piece cache from content so an offline
// torrent has data to read. It replaces the on-disk payload fixture the old
// file-backed storage used.
func seedPieces(t *testing.T, sess *session.Session, hash metainfo.Hash, content []byte) {
	t.Helper()
	if err := sess.SeedPiecesForTest(hash, content); err != nil {
		t.Fatalf("seed pieces: %v", err)
	}
}

// waitCached blocks until the torrent's whole length is resident in the piece
// cache, which is what "the data is available" means now that pieces are not
// persisted.
func waitCached(t *testing.T, ctx context.Context, st *session.Torrent) {
	t.Helper()
	select {
	case <-st.GotInfo():
	case <-ctx.Done():
		t.Fatalf("timed out waiting for torrent info")
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if st.CachedBytes() == st.Length() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("torrent cache never filled: %d/%d bytes", st.CachedBytes(), st.Length())
}

// buildSingleFileTorrent produces a .torrent file that describes data. It writes
// no data anywhere: a test feeds the pieces through seedPieces once the torrent
// is registered.
func buildSingleFileTorrent(t *testing.T, torrentDir, name string, data []byte) (torrentPath string, hash metainfo.Hash) {
	t.Helper()

	torrentBytes, hash := buildSingleFileTorrentBytes(t, name, data, nil)
	torrentPath = filepath.Join(torrentDir, name+".torrent")
	if err := os.WriteFile(torrentPath, torrentBytes, 0o644); err != nil {
		t.Fatalf("write torrent: %v", err)
	}
	return torrentPath, hash
}

// buildSingleFileTorrentBytes encodes a single-file .torrent describing data.
// Tracker tiers are embedded verbatim, so a test can point the torrent at a
// loopback tracker without touching global config.
func buildSingleFileTorrentBytes(t *testing.T, name string, data []byte, trackers [][]string) ([]byte, metainfo.Hash) {
	t.Helper()
	pieces := make([]byte, 0, (len(data)+testPieceLength-1)/testPieceLength*sha1.Size)
	for off := 0; off < len(data); off += testPieceLength {
		end := off + testPieceLength
		if end > len(data) {
			end = len(data)
		}
		sum := sha1.Sum(data[off:end])
		pieces = append(pieces, sum[:]...)
	}
	info := metainfo.Info{
		Name:        name,
		Length:      int64(len(data)),
		PieceLength: testPieceLength,
		Pieces:      pieces,
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatalf("encode info: %v", err)
	}
	mi := metainfo.MetaInfo{InfoBytes: bencode.Bytes(infoBytes), AnnounceList: trackers}
	if len(trackers) > 0 && len(trackers[0]) > 0 {
		mi.Announce = trackers[0][0]
	}
	torrentBytes, err := bencode.Marshal(mi)
	if err != nil {
		t.Fatalf("encode metainfo: %v", err)
	}
	return torrentBytes, mi.HashInfoBytes()
}

func buildMultiFileTorrent(t *testing.T, torrentDir, name string, files map[string][]byte) (torrentPath string, hash metainfo.Hash, all []byte) {
	t.Helper()
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	infoFiles := make([]metainfo.FileInfo, 0, len(paths))
	for _, path := range paths {
		data := files[path]
		infoFiles = append(infoFiles, metainfo.FileInfo{
			Length: int64(len(data)),
			Path:   strings.Split(path, "/"),
		})
		all = append(all, data...)
	}
	pieces := make([]byte, 0, (len(all)+testPieceLength-1)/testPieceLength*sha1.Size)
	for off := 0; off < len(all); off += testPieceLength {
		end := off + testPieceLength
		if end > len(all) {
			end = len(all)
		}
		sum := sha1.Sum(all[off:end])
		pieces = append(pieces, sum[:]...)
	}
	info := metainfo.Info{
		Name:        name,
		PieceLength: testPieceLength,
		Pieces:      pieces,
		Files:       infoFiles,
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatalf("encode multi-file info: %v", err)
	}
	mi := metainfo.MetaInfo{InfoBytes: bencode.Bytes(infoBytes)}
	torrentBytes, err := bencode.Marshal(mi)
	if err != nil {
		t.Fatalf("encode multi-file metainfo: %v", err)
	}
	hash = mi.HashInfoBytes()
	torrentPath = filepath.Join(torrentDir, name+".torrent")
	if err := os.WriteFile(torrentPath, torrentBytes, 0o644); err != nil {
		t.Fatalf("write multi-file torrent: %v", err)
	}
	return torrentPath, hash, all
}
