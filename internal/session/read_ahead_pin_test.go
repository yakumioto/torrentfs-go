package session

import (
	"context"
	"sync"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/cache"
)

type orderedPieceSource struct {
	mu     sync.Mutex
	starts []int64
}

func (s *orderedPieceSource) ReadAtContext(ctx context.Context, dst []byte, off, _ int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	s.starts = append(s.starts, off)
	s.mu.Unlock()
	for i := range dst {
		dst[i] = byte((off + int64(i)) % 251)
	}
	return len(dst), nil
}

func (s *orderedPieceSource) Close() error { return nil }

// TestRaFileReadWithOutOfOrderReadAheadPieces ensures future pieces that arrived
// before the requested piece cannot consume all pin budget. The target piece
// must be insertable even when the read-ahead window includes future pieces in a
// two-piece cache.
func TestRaFileReadWithOutOfOrderReadAheadPieces(t *testing.T) {
	const pieceLength = int64(8)
	store := cache.New(2 * pieceLength)
	source := &orderedPieceSource{}
	file := &raFile{
		loader:      source,
		cache:       store,
		torrentKey:  "unordered",
		fileSize:    3 * pieceLength,
		pieceLength: pieceLength,
		torrentSize: 3 * pieceLength,
		readahead:   2 * pieceLength,
	}

	request := cache.ReadRequest{FileSize: file.fileSize, PieceLength: pieceLength, TorrentLength: file.torrentSize}
	file.protectWindow(file.windowKeys(request, 3*pieceLength))
	// The future pieces arrive after the read-ahead keys have been pinned but
	// before the target loader completes: this is the legal out-of-order timing
	// that used to strand the target insertion.
	for index := 1; index <= 2; index++ {
		data := make([]byte, pieceLength)
		for i := range data {
			data[i] = byte((int64(index)*pieceLength + int64(i)) % 251)
		}
		store.Put(cache.Key{Torrent: "unordered", Piece: index}, data)
	}
	got, err := file.piece(context.Background(), source, 0)
	if err != nil || len(got) != int(pieceLength) {
		t.Fatalf("target read = (%d bytes, %v), want (%d bytes, nil)", len(got), err, pieceLength)
	}
	for i, b := range got {
		if want := byte(int64(i) % 251); b != want {
			t.Fatalf("target byte %d = %d, want %d", i, b, want)
		}
	}
	if !store.Has(cache.Key{Torrent: "unordered", Piece: 0}) {
		t.Fatal("target piece was not inserted after future pieces arrived first")
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if len(source.starts) != 1 || source.starts[0] != 0 {
		t.Fatalf("loader starts = %v, want exactly one target read at offset 0", source.starts)
	}
}

// TestRaFileTargetPinFailureDoesNotPinShortTail covers the final-piece case:
// when a full target piece cannot fit the pin budget, a smaller future tail must
// not be pinned ahead of it.
func TestRaFileTargetPinFailureDoesNotPinShortTail(t *testing.T) {
	const pieceLength = int64(8)
	store := cache.New(pieceLength)
	source := &orderedPieceSource{}
	file := &raFile{
		loader:      source,
		cache:       store,
		torrentKey:  "short-tail",
		fileSize:    pieceLength + 2,
		pieceLength: pieceLength,
		torrentSize: pieceLength + 2,
		readahead:   2,
	}
	request := cache.ReadRequest{FileSize: file.fileSize, PieceLength: pieceLength, TorrentLength: file.torrentSize}
	file.protectWindow(file.windowKeys(request, pieceLength+2))
	store.Put(cache.Key{Torrent: "short-tail", Piece: 1}, []byte("ta"))
	if _, err := file.piece(context.Background(), source, 0); err != nil {
		t.Fatalf("target read = %v", err)
	}
	if !store.Has(cache.Key{Torrent: "short-tail", Piece: 0}) {
		t.Fatal("target piece was not inserted when the short tail arrived first")
	}
}
