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

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

func openWarmSession(t *testing.T, dataDir, torrentPath string, hash metainfo.Hash) *session.Session {
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	waitComplete(t, ctx, st)
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
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", content)
	sess := openWarmSession(t, dataDir, torrentPath, hash)

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
	torrentPath, hash, _ := buildMultiFileTorrent(t, dataDir, work, "multi", files)
	sess := openWarmSession(t, dataDir, torrentPath, hash)

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

func TestSessionPieceStatesCompleteTorrent(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	content := []byte(strings.Repeat("state ", (testPieceLength/5)+1))
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", content)
	sess := openWarmSession(t, dataDir, torrentPath, hash)

	states, err := sess.PieceStates(hash)
	if err != nil {
		t.Fatalf("PieceStates: %v", err)
	}
	numPieces := (len(content) + testPieceLength - 1) / testPieceLength
	if len(states) != numPieces {
		t.Fatalf("len(states) = %d, want %d", len(states), numPieces)
	}
	for i, state := range states {
		if !state.Known || !state.Complete {
			t.Fatalf("piece %d state = %+v, want complete", i, state)
		}
	}
}

func TestSessionPieceStatesUnknownTorrent(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	sess, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if _, err := sess.PieceStates(metainfo.Hash{}); !errors.Is(err, filesystem.ErrNotFound) {
		t.Fatalf("PieceStates error = %v, want ErrNotFound", err)
	}
}

// TestSessionFilePieceStatesSlicesPerFile checks the per-file projection the
// stats/ mirror renders: each file reports exactly the pieces that back its
// byte range, and boundary pieces shared with a neighbour are reported by both
// files without being duplicated or dropped.
func TestSessionFilePieceStatesSlicesPerFile(t *testing.T) {
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	files := map[string][]byte{
		"first.bin":  []byte(strings.Repeat("A", 128<<10)),
		"second.bin": []byte(strings.Repeat("B", 200<<10)),
		"third.bin":  []byte("tail"),
	}
	torrentPath, hash, _ := buildMultiFileTorrent(t, dataDir, work, "multi", files)
	sess := openWarmSession(t, dataDir, torrentPath, hash)

	whole, err := sess.PieceStates(hash)
	if err != nil {
		t.Fatalf("PieceStates: %v", err)
	}
	if len(whole) != 2 {
		t.Fatalf("whole snapshot = %d pieces, want 2", len(whole))
	}

	// first.bin [0, 128KiB) lies inside piece 0; second.bin [128KiB, 328KiB)
	// spans pieces 0 and 1; third.bin [328KiB, 328KiB+4) lies in piece 1.
	wantPieces := map[string]int{"first.bin": 1, "second.bin": 2, "third.bin": 1}
	for path, want := range wantPieces {
		got, err := sess.FilePieceStates(hash, path)
		if err != nil {
			t.Fatalf("FilePieceStates(%s): %v", path, err)
		}
		if len(got) != want {
			t.Fatalf("FilePieceStates(%s) = %d pieces, want %d", path, len(got), want)
		}
		for i, state := range got {
			if !state.Known || !state.Complete {
				t.Fatalf("FilePieceStates(%s)[%d] = %+v, want complete", path, i, state)
			}
		}
	}

	// The single-file projection equals the whole-torrent snapshot.
	singleDataDir := filepath.Join(work, "single-data")
	singlePath, singleHash := buildSingleFileTorrent(t, singleDataDir, work, "payload.bin", []byte(strings.Repeat("x", testPieceLength+8)))
	single := openWarmSession(t, singleDataDir, singlePath, singleHash)
	whole, err = single.PieceStates(singleHash)
	if err != nil {
		t.Fatalf("single PieceStates: %v", err)
	}
	projected, err := single.FilePieceStates(singleHash, "payload.bin")
	if err != nil {
		t.Fatalf("single FilePieceStates: %v", err)
	}
	if len(projected) != len(whole) {
		t.Fatalf("single-file projection = %d pieces, want %d (whole)", len(projected), len(whole))
	}

	if _, err := sess.FilePieceStates(hash, "missing.bin"); !errors.Is(err, filesystem.ErrNotFound) {
		t.Fatalf("FilePieceStates(missing) = %v, want ErrNotFound", err)
	}
	if _, err := sess.FilePieceStates(metainfo.Hash{}, "first.bin"); !errors.Is(err, filesystem.ErrNotFound) {
		t.Fatalf("FilePieceStates(unknown torrent) = %v, want ErrNotFound", err)
	}
}
