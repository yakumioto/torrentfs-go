package session

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
)

func TestReaderReadaheadCandidateMatrix(t *testing.T) {
	const (
		pieceLength = int64(internalTestPieceLength)
		pieceCount  = 160
		targetPiece = 8
	)

	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	content := make([]byte, pieceLength*pieceCount)
	for i := range content {
		content[i] = byte(i*31 + i/251 + 1)
	}
	torrentPath, hash := buildInternalTestTorrentWithPieceLength(t, dataDir, work, content, pieceLength)
	payloadPath := filepath.Join(dataDir, "payload", hash.HexString(), "payload.bin")
	if err := os.Remove(payloadPath); err != nil {
		t.Fatalf("remove complete payload: %v", err)
	}
	payload, err := os.OpenFile(payloadPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("create sparse payload: %v", err)
	}
	targetStart := int64(targetPiece) * pieceLength
	if err := payload.Truncate(targetStart + pieceLength); err != nil {
		_ = payload.Close()
		t.Fatalf("truncate sparse payload: %v", err)
	}
	if _, err := payload.WriteAt(content[targetStart:targetStart+pieceLength], targetStart); err != nil {
		_ = payload.Close()
		t.Fatalf("write target piece: %v", err)
	}
	if err := payload.Close(); err != nil {
		t.Fatalf("close sparse payload: %v", err)
	}

	sess, err := New(internalTestConfig(dataDir), internalTestTorrentDir(t, dataDir))
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
	if err := sess.AddTorrent(ctx, Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	select {
	case <-st.GotInfo():
	case <-ctx.Done():
		t.Fatalf("torrent info: %v", ctx.Err())
	}
	deadline := time.Now().Add(10 * time.Second)
	for st.BytesCompleted() < pieceLength && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := st.BytesCompleted(); got < pieceLength {
		t.Fatalf("target piece did not become complete: %d/%d bytes", got, pieceLength)
	}

	candidates := []struct {
		name  string
		bytes int64
	}{
		{name: "piece-length baseline", bytes: pieceLength},
		{name: "8 MiB", bytes: 8 << 20},
		{name: "16 MiB", bytes: 16 << 20},
		{name: "32 MiB", bytes: 32 << 20},
	}
	positions := []struct {
		name   string
		offset int64
	}{
		{name: "piece boundary", offset: targetStart},
		{name: "piece middle", offset: targetStart + pieceLength/2},
	}
	for _, position := range positions {
		position := position
		t.Run(position.name, func(t *testing.T) {
			for _, candidate := range candidates {
				candidate := candidate
				t.Run(candidate.name, func(t *testing.T) {
					runReadaheadCandidate(t, st, content, pieceLength, position.offset, candidate.bytes)
				})
			}
		})
	}
}

func runReadaheadCandidate(t *testing.T, st *Torrent, content []byte, pieceLength, offset, readahead int64) {
	t.Helper()
	reader := st.tor.NewReader()
	ctx, cancel := context.WithCancel(context.Background())
	reader.SetContext(ctx)
	reader.SetReadahead(readahead)
	if _, err := reader.Seek(offset, io.SeekStart); err != nil {
		cancel()
		_ = reader.Close()
		t.Fatalf("Seek: %v", err)
	}

	readStarted := time.Now()
	readDone := make(chan struct {
		n    int
		err  error
		data []byte
	}, 1)
	go func() {
		buf := make([]byte, 64)
		n, err := reader.Read(buf)
		readDone <- struct {
			n    int
			err  error
			data []byte
		}{n: n, err: err, data: buf}
	}()
	select {
	case result := <-readDone:
		if result.err != nil || result.n != 64 {
			cancel()
			_ = reader.Close()
			t.Fatalf("first read = %d bytes, %v; want 64, nil", result.n, result.err)
		}
		if want := content[int(offset) : int(offset)+64]; !bytes.Equal(result.data, want) {
			cancel()
			_ = reader.Close()
			t.Fatalf("first read data differs from target piece")
		}
	case <-time.After(5 * time.Second):
		cancel()
		_ = reader.Close()
		t.Fatal("first read did not return")
	}

	readOffset := offset + 64
	wantForward := int((readOffset+readahead+pieceLength-1)/pieceLength-readOffset/pieceLength) - 1
	forwardDeadline := time.Now().Add(time.Second)
	gotForward := 0
	for time.Now().Before(forwardDeadline) {
		gotForward = readaheadPieces(st.tor.PieceStateRuns())
		if gotForward >= wantForward {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if gotForward != wantForward {
		cancel()
		_ = reader.Close()
		t.Fatalf("forward priority pieces = %d, want %d", gotForward, wantForward)
	}
	t.Logf("first_data=%s forward_pieces=%d forward_bytes=%d", time.Since(readStarted), gotForward, int64(gotForward)*pieceLength)

	cancel()
	if err := reader.Close(); err != nil {
		t.Fatalf("Close reader: %v", err)
	}
}

func readaheadPieces(runs torrent.PieceStateRuns) int {
	pieces := 0
	for _, run := range runs {
		if run.Priority == torrent.PiecePriorityReadahead {
			pieces += run.Length
		}
	}
	return pieces
}
