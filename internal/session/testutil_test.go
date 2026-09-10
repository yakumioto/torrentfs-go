package session_test

import (
	"crypto/sha1"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

const testPieceLength = 256 << 10

func testConfig(dataDir string) config.Config {
	cfg := config.Default()
	cfg.Paths.DataDir = dataDir
	return cfg
}

// buildSingleFileTorrent writes data into the session data directory (using
// anacrolix's default file storage layout: DataDir/<info name>) and produces
// a .torrent file that describes it. The torrent has no trackers, so nothing
// in the test touches the network.
func buildSingleFileTorrent(t *testing.T, dataDir, torrentDir, name string, data []byte) (torrentPath string, hash metainfo.Hash) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("make data dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, name), data, 0o644); err != nil {
		t.Fatalf("write data file: %v", err)
	}

	torrentBytes, hash := buildSingleFileTorrentBytes(t, name, data, nil)
	torrentPath = filepath.Join(torrentDir, name+".torrent")
	if err := os.WriteFile(torrentPath, torrentBytes, 0o644); err != nil {
		t.Fatalf("write torrent: %v", err)
	}
	return torrentPath, hash
}

// buildSingleFileTorrentBytes encodes a single-file .torrent describing data
// laid out as DataDir/<name>. Tracker tiers are embedded verbatim, so a test
// can point the torrent at a loopback tracker without touching global config.
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

func buildMultiFileTorrent(t *testing.T, dataDir, torrentDir, name string, files map[string][]byte) (torrentPath string, hash metainfo.Hash, all []byte) {
	t.Helper()
	root := filepath.Join(dataDir, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("make multi-file root: %v", err)
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	infoFiles := make([]metainfo.FileInfo, 0, len(paths))
	for _, path := range paths {
		data := files[path]
		fullPath := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatalf("make parent for %q: %v", path, err)
		}
		if err := os.WriteFile(fullPath, data, 0o644); err != nil {
			t.Fatalf("write %q: %v", path, err)
		}
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
	torrentPath = filepath.Join(torrentDir, name+".torrent")
	if err := os.WriteFile(torrentPath, torrentBytes, 0o644); err != nil {
		t.Fatalf("write multi-file torrent: %v", err)
	}
	return torrentPath, mi.HashInfoBytes(), all
}
