package piecestore

import (
	"context"
	"crypto/sha1"
	"errors"
	"io"
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
