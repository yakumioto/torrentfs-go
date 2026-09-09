package session_test

import (
	"crypto/sha1"
	"os"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

const testPieceLength = 256 << 10

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
	mi := metainfo.MetaInfo{InfoBytes: bencode.Bytes(infoBytes)}
	torrentBytes, err := bencode.Marshal(mi)
	if err != nil {
		t.Fatalf("encode metainfo: %v", err)
	}
	torrentPath = filepath.Join(torrentDir, name+".torrent")
	if err := os.WriteFile(torrentPath, torrentBytes, 0o644); err != nil {
		t.Fatalf("write torrent: %v", err)
	}
	return torrentPath, mi.HashInfoBytes()
}
