package piecestore

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"io"
	"sync"
	"testing"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/yakumioto/torrentfs-go/internal/cache"
)

func testStore(t *testing.T, capacity int64) (*Store, *cache.Cache) {
	t.Helper()
	c := cache.New(capacity)
	return New(c, nil), c
}

// openPiece opens a single-piece torrent whose only piece is pieceLength bytes,
// and returns the PieceImpl the store hands anacrolix for it.
func openPiece(t *testing.T, store *Store, hash metainfo.Hash, pieceLength int64) (cache.Key, storage.PieceImpl) {
	t.Helper()
	info := singlePieceInfo(pieceLength)
	impl, err := store.OpenTorrent(context.Background(), info, hash)
	if err != nil {
		t.Fatalf("OpenTorrent: %v", err)
	}
	piece := info.Piece(0)
	return cache.Key{Torrent: hash.HexString(), Piece: 0}, impl.PieceWithHash(piece, g.None[[]byte]())
}

// singlePieceInfo returns a one-piece, single-file v1 info. The zero-value
// Info cannot be used directly: its length helpers walk the file list.
func singlePieceInfo(pieceLength int64) *metainfo.Info {
	return &metainfo.Info{
		Name:        "t",
		PieceLength: pieceLength,
		Files:       []metainfo.FileInfo{{Length: pieceLength, Path: []string{"t"}}},
		Pieces:      make([]byte, sha1.Size),
	}
}

func TestOpenTorrentRejectsPieceLargerThanCache(t *testing.T) {
	store, _ := testStore(t, 512)
	info := singlePieceInfo(1024)
	if _, err := store.OpenTorrent(context.Background(), info, metainfo.Hash{1}); err == nil {
		t.Fatal("OpenTorrent accepted a piece larger than the cache")
	}
}

func TestOpenTorrentAcceptsPieceWithinCapacity(t *testing.T) {
	store, _ := testStore(t, 4096)
	info := singlePieceInfo(1024)
	if _, err := store.OpenTorrent(context.Background(), info, metainfo.Hash{1}); err != nil {
		t.Fatalf("OpenTorrent: %v", err)
	}
}

func TestPieceMissReportsEOF(t *testing.T) {
	const pieceLength = 16
	store, _ := testStore(t, 1<<20)
	key, piece := openPiece(t, store, metainfo.Hash{2}, pieceLength)

	if n, err := piece.ReadAt(make([]byte, 4), 0); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read of missing piece = (%d, %v), want (0, io.EOF)", n, err)
	}
	completion := piece.Completion()
	if !completion.Ok || completion.Complete || completion.Err != nil {
		t.Fatalf("missing piece completion = %+v, want known-incomplete", completion)
	}
	_ = key
}

func TestPieceWriteReadCompleteLifecycle(t *testing.T) {
	const pieceLength = 8
	store, c := testStore(t, 1<<20)
	key, piece := openPiece(t, store, metainfo.Hash{3}, pieceLength)

	// anacrolix writes chunk by chunk; each write must report a full write.
	if n, err := piece.WriteAt([]byte("abcd"), 0); n != 4 || err != nil {
		t.Fatalf("WriteAt = (%d, %v), want (4, nil)", n, err)
	}
	if n, err := piece.WriteAt([]byte("efgh"), 4); n != 4 || err != nil {
		t.Fatalf("second WriteAt = (%d, %v), want (4, nil)", n, err)
	}
	// Staged data is readable before the hash check marks the piece complete.
	got := make([]byte, 4)
	if n, err := piece.ReadAt(got, 4); n != 4 || err != nil || string(got) != "efgh" {
		t.Fatalf("staging read = (%d, %v, %q), want (4, nil, efgh)", n, err, got)
	}

	if err := piece.MarkComplete(); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	if completion := piece.Completion(); !completion.Ok || !completion.Complete {
		t.Fatalf("completion after MarkComplete = %+v, want complete", completion)
	}
	if !c.Has(key) {
		t.Fatal("MarkComplete did not promote the piece into the LRU")
	}
	if c.SizeOf(key.Torrent) != pieceLength {
		t.Fatalf("SizeOf = %d, want %d", c.SizeOf(key.Torrent), pieceLength)
	}

	if err := piece.MarkNotComplete(); err != nil {
		t.Fatalf("MarkNotComplete: %v", err)
	}
	if completion := piece.Completion(); !completion.Ok || completion.Complete {
		t.Fatalf("completion after MarkNotComplete = %+v, want known-incomplete", completion)
	}
	if c.Has(key) {
		t.Fatal("MarkNotComplete left the piece cached")
	}
}

func TestResidentPieceIsServedInFull(t *testing.T) {
	const pieceLength = 8
	store, c := testStore(t, 1<<20)
	key, piece := openPiece(t, store, metainfo.Hash{4}, pieceLength)
	c.Put(key, []byte("abcdefgh"))

	buf := make([]byte, pieceLength)
	n, err := piece.ReadAt(buf, 0)
	if n != pieceLength || err != nil || string(buf) != "abcdefgh" {
		t.Fatalf("resident read = (%d, %v, %q), want full data", n, err, buf)
	}

	short := make([]byte, 4)
	if n, err := piece.ReadAt(short, 0); n != 4 || err != nil || string(short) != "abcd" {
		t.Fatalf("windowed read = (%d, %v, %q), want abcd", n, err, short)
	}
	if _, err := piece.ReadAt(make([]byte, 4), pieceLength); !errors.Is(err, io.EOF) {
		t.Fatalf("read at the piece end = %v, want io.EOF", err)
	}
}

// TestReadAtPastPieceEndReturnsEOF pins the io.ReaderAt contract: a buffer that
// runs past the piece end must come back with a non-nil error, never as a short
// read that looks successful.
func TestReadAtPastPieceEndReturnsEOF(t *testing.T) {
	const pieceLength = 8
	store, c := testStore(t, 1<<20)
	key, piece := openPiece(t, store, metainfo.Hash{7}, pieceLength)
	c.Put(key, []byte("abcdefgh"))

	buf := make([]byte, pieceLength+8)
	n, err := piece.ReadAt(buf, 0)
	if n != pieceLength {
		t.Fatalf("n = %d, want %d", n, pieceLength)
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF for a buffer past the piece end", err)
	}
	if string(buf[:n]) != "abcdefgh" {
		t.Fatalf("data = %q, want abcdefgh", buf[:n])
	}

	// The same rule for a window that ends before the buffer does.
	partial := make([]byte, 12)
	n, err = piece.ReadAt(partial, 4)
	if n != 4 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt(4, 12) = (%d, %v), want (4, io.EOF)", n, err)
	}
	if string(partial[:n]) != "efgh" {
		t.Fatalf("data = %q, want efgh", partial[:n])
	}
}

func TestReadAtPastPieceEndFromStagingReturnsEOF(t *testing.T) {
	const pieceLength = 8
	store, _ := testStore(t, 1<<20)
	_, piece := openPiece(t, store, metainfo.Hash{8}, pieceLength)
	if _, err := piece.WriteAt([]byte("abcdefgh"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	buf := make([]byte, 12)
	n, err := piece.ReadAt(buf, 4)
	if n != 4 || !errors.Is(err, io.EOF) {
		t.Fatalf("staging ReadAt = (%d, %v), want (4, io.EOF)", n, err)
	}
	if string(buf[:n]) != "efgh" {
		t.Fatalf("staging data = %q, want efgh", buf[:n])
	}
}

// TestReadAtConcurrentWithWriteAt drives readers and a writer over one piece at
// the same time. ReadAt copies the staging window under the store lock, so this
// pairing is synchronized; when stagingBytes handed the buffer out instead, the
// copy raced the writer on the same backing array and the race detector
// reported it here.
func TestReadAtConcurrentWithWriteAt(t *testing.T) {
	const pieceLength = 64
	store, _ := testStore(t, 1<<20)
	_, piece := openPiece(t, store, metainfo.Hash{9}, pieceLength)

	chunk := bytes.Repeat([]byte("w"), 16)
	fill := func() {
		for off := int64(0); off < pieceLength; off += int64(len(chunk)) {
			if _, err := piece.WriteAt(chunk, off); err != nil {
				t.Errorf("WriteAt: %v", err)
				return
			}
		}
	}
	// Give the piece a staging buffer first. The test is about the read copy
	// racing the writer, not about the state before the first write: with no
	// buffer at all a read correctly reports the piece as missing.
	fill()

	done := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			fill()
		}
		close(done)
	}()

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, pieceLength)
			for {
				select {
				case <-done:
					return
				default:
				}
				if n, err := piece.ReadAt(buf, 0); n != pieceLength || err != nil {
					t.Errorf("ReadAt = (%d, %v), want (%d, nil)", n, err, pieceLength)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestStagingReadRejectsUnreceivedWindows pins the contract that keeps a
// streaming read honest: a window that includes bytes a peer has not delivered
// yet reports the piece as missing. Returning the zero fill instead let
// anacrolix's reader copy unwritten bytes into the file.
func TestStagingReadRejectsUnreceivedWindows(t *testing.T) {
	const pieceLength = 8
	store, _ := testStore(t, 1<<20)
	_, piece := openPiece(t, store, metainfo.Hash{10}, pieceLength)

	// Only the second half has arrived.
	if _, err := piece.WriteAt([]byte("efgh"), 4); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	for _, tc := range []struct {
		name string
		off  int64
		size int64
	}{
		{name: "window covering the gap", off: 0, size: pieceLength},
		{name: "window ending in the gap", off: 0, size: 6},
		{name: "window straddling the boundary", off: 2, size: 4},
	} {
		buf := bytes.Repeat([]byte{'?'}, int(tc.size))
		n, err := piece.ReadAt(buf, tc.off)
		if n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("%s: ReadAt(%d, %d) = (%d, %v, %q), want (0, io.EOF)",
				tc.name, tc.off, tc.size, n, err, buf[:n])
		}
	}

	// A window fully inside the received range is still served before the hash
	// check promotes the piece.
	buf := make([]byte, 4)
	n, err := piece.ReadAt(buf, 4)
	if n != 4 || err != nil || string(buf[:n]) != "efgh" {
		t.Fatalf("received window = (%d, %v, %q), want (4, nil, efgh)", n, err, buf[:n])
	}
}

// TestStagingCoverageMergesChunks checks that out-of-order chunk arrival builds
// a coverage map a read can trust, including adjacent and overlapping writes.
func TestStagingCoverageMergesChunks(t *testing.T) {
	const pieceLength = 16
	store, _ := testStore(t, 1<<20)
	_, piece := openPiece(t, store, metainfo.Hash{11}, pieceLength)

	writes := []struct {
		off  int64
		data string
	}{
		{off: 8, data: "ijklmnop"}, // the tail arrives first
		{off: 0, data: "abcdefgh"}, // then the head, adjacent to the tail
	}
	for _, w := range writes {
		if _, err := piece.WriteAt([]byte(w.data), w.off); err != nil {
			t.Fatalf("WriteAt(%d): %v", w.off, err)
		}
	}

	buf := make([]byte, pieceLength)
	n, err := piece.ReadAt(buf, 0)
	if n != pieceLength || err != nil || string(buf[:n]) != "abcdefghijklmnop" {
		t.Fatalf("merged coverage read = (%d, %v, %q), want the whole piece", n, err, buf[:n])
	}
}

func TestWriteAtOutsidePieceIsRejected(t *testing.T) {
	store, _ := testStore(t, 1<<20)
	_, piece := openPiece(t, store, metainfo.Hash{5}, 8)
	if _, err := piece.WriteAt([]byte("toolong!!"), 0); err == nil {
		t.Fatal("WriteAt accepted a write past the piece length")
	}
}

func TestCloseDropsStaging(t *testing.T) {
	store, _ := testStore(t, 1<<20)
	_, piece := openPiece(t, store, metainfo.Hash{6}, 4)
	if _, err := piece.WriteAt([]byte("data"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := piece.WriteAt([]byte("data"), 0); err == nil {
		t.Fatal("WriteAt after Close succeeded")
	}
}
