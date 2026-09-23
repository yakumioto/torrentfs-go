package session_test

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

func openWarmSession(t *testing.T, dataDir, torrentPath string, hash metainfo.Hash, content []byte) *session.Session {
	t.Helper()
	sess, err := session.New(testConfig(), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	seedPieces(t, sess, hash, content)
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	waitCached(t, ctx, st)
	return sess
}

func readAllAt(ra io.ReaderAt, content []byte) error {
	for off := 0; off < len(content); {
		buf := make([]byte, 8192)
		if rest := len(content) - off; rest < len(buf) {
			buf = buf[:rest]
		}
		n, err := ra.ReadAt(buf, int64(off))
		if err != nil && err != io.EOF {
			return err
		}
		if n == 0 {
			if err == io.EOF {
				break
			}
			return io.ErrUnexpectedEOF
		}
		off += n
	}
	return nil
}

func TestSessionPieceCacheServesReads(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	content := []byte(strings.Repeat("piececache", (testPieceLength/10)+1))
	torrentPath, hash := buildSingleFileTorrent(t, work, "payload.bin", content)
	sess := openWarmSession(t, dataDir, torrentPath, hash, content)

	ra, err := sess.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if err := readAllAt(ra, content); err != nil {
		t.Fatalf("first full read: %v", err)
	}
	buf := make([]byte, 32)
	if _, err := ra.ReadAt(buf, 1); err != nil {
		t.Fatalf("mid read: %v", err)
	}
	if string(buf) != string(content[1:33]) {
		t.Fatalf("mid read = %q, want %q", buf, content[1:33])
	}
	if err := readAllAt(ra, content); err != nil {
		t.Fatalf("second full read: %v", err)
	}
}

func TestSessionPieceCacheReadsAcrossFileBoundary(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	files := map[string][]byte{
		"first.bin":  []byte(strings.Repeat("A", 128<<10)),
		"second.bin": []byte(strings.Repeat("B", 200<<10)),
		"third.bin":  []byte("tail"),
	}
	torrentPath, hash, all := buildMultiFileTorrent(t, work, "multi", files)
	sess := openWarmSession(t, dataDir, torrentPath, hash, all)

	ra, err := sess.OpenFile(hash, "second.bin")
	if err != nil {
		t.Fatalf("OpenFile(second.bin): %v", err)
	}
	got := make([]byte, len(files["second.bin"]))
	if _, err := io.ReadFull(io.NewSectionReader(ra, 0, int64(len(got))), got); err != nil {
		t.Fatalf("read second.bin: %v", err)
	}
	if string(got) != string(files["second.bin"]) {
		t.Fatal("cross-file piece read differs from source")
	}
}

func TestTorrentStatusCompleteSnapshot(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	content := []byte(strings.Repeat("state ", (testPieceLength/5)+1))
	torrentPath, hash := buildSingleFileTorrent(t, work, "payload.bin", content)
	sess := openWarmSession(t, dataDir, torrentPath, hash, content)

	status, err := sess.TorrentStatusFor(hash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor: %v", err)
	}
	if !status.MetainfoReady || status.PieceLength != testPieceLength {
		t.Fatalf("status = %+v, want metainfo and piece length", status)
	}
	wantPieces := (len(content) + testPieceLength - 1) / testPieceLength
	if len(status.Pieces) != wantPieces {
		t.Fatalf("pieces = %d, want %d", len(status.Pieces), wantPieces)
	}
	for index, piece := range status.Pieces {
		if piece.Index != index || !piece.Cached || piece.CachedBytes <= 0 {
			t.Fatalf("piece %d = %+v, want cached piece", index, piece)
		}
	}
	if len(status.Files) != 1 || status.Files[0].Path != "payload.bin" || status.Files[0].PieceStart != 0 || status.Files[0].PieceEnd != wantPieces {
		t.Fatalf("files = %+v, want payload range [0,%d)", status.Files, wantPieces)
	}
}

func TestTorrentStatusSharesBoundaryPieces(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	files := map[string][]byte{
		"first.bin":  []byte(strings.Repeat("A", 128<<10)),
		"second.bin": []byte(strings.Repeat("B", 200<<10)),
		"third.bin":  []byte("tail"),
	}
	torrentPath, hash, all := buildMultiFileTorrent(t, work, "multi", files)
	sess := openWarmSession(t, dataDir, torrentPath, hash, all)

	status, err := sess.TorrentStatusFor(hash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor: %v", err)
	}
	if len(status.Pieces) != 2 {
		t.Fatalf("pieces = %d, want 2", len(status.Pieces))
	}
	byPath := make(map[string]session.FileStatus, len(status.Files))
	for _, file := range status.Files {
		byPath[file.Path] = file
	}
	want := map[string][2]int{
		"first.bin":  {0, 1},
		"second.bin": {0, 2},
		"third.bin":  {1, 2},
	}
	for path, interval := range want {
		file, ok := byPath[path]
		if !ok || file.PieceStart != interval[0] || file.PieceEnd != interval[1] {
			t.Fatalf("file %q = %+v, want [%d,%d)", path, file, interval[0], interval[1])
		}
	}
}

func TestTorrentStatusSelectivePiecesKeepFileRanges(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	files := map[string][]byte{
		"first.bin":  []byte(strings.Repeat("A", 128<<10)),
		"second.bin": []byte(strings.Repeat("B", 200<<10)),
		"third.bin":  []byte("tail"),
	}
	torrentPath, hash, all := buildMultiFileTorrent(t, work, "multi", files)
	sess, err := session.New(testConfig(), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}

	if err := sess.SeedPieceForTest(hash, 1, all); err != nil {
		t.Fatalf("SeedPieceForTest(1): %v", err)
	}
	status, err := sess.TorrentStatusFor(hash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor after piece 1: %v", err)
	}
	if len(status.Pieces) != 2 || status.Pieces[0].Cached || !status.Pieces[1].Cached {
		t.Fatalf("selective pieces = %+v, want only piece 1 cached", status.Pieces)
	}
	byPath := make(map[string]session.FileStatus, len(status.Files))
	for _, file := range status.Files {
		byPath[file.Path] = file
	}
	if first := byPath["first.bin"]; first.PieceStart != 0 || first.PieceEnd != 1 {
		t.Fatalf("first.bin range = %+v, want [0,1)", first)
	}
	if second := byPath["second.bin"]; second.PieceStart != 0 || second.PieceEnd != 2 {
		t.Fatalf("second.bin range = %+v, want [0,2)", second)
	}
	if third := byPath["third.bin"]; third.PieceStart != 1 || third.PieceEnd != 2 {
		t.Fatalf("third.bin range = %+v, want [1,2)", third)
	}
	pieceOneBytes := int64(len(all)) - int64(testPieceLength)
	if status.Torrent.CachedBytes != pieceOneBytes {
		t.Fatalf("cached_bytes after piece 1 = %d, want %d", status.Torrent.CachedBytes, pieceOneBytes)
	}

	if err := sess.SeedPieceForTest(hash, 0, all); err != nil {
		t.Fatalf("SeedPieceForTest(0): %v", err)
	}
	status, err = sess.TorrentStatusFor(hash.HexString())
	if err != nil {
		t.Fatalf("TorrentStatusFor after piece 0: %v", err)
	}
	if status.Torrent.CachedBytes != int64(testPieceLength)+pieceOneBytes {
		t.Fatalf("cached_bytes after both pieces = %d, want %d", status.Torrent.CachedBytes, int64(testPieceLength)+pieceOneBytes)
	}
}

func TestTorrentStatusUnknownTorrent(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	sess, err := session.New(testConfig(), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if _, err := sess.TorrentStatusFor(metainfo.Hash{}.HexString()); !errors.Is(err, session.ErrUnknownTorrent) {
		t.Fatalf("TorrentStatusFor error = %v, want ErrUnknownTorrent", err)
	}
}
