package session

import (
	"context"
	"sync"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/cache"
)

// countingPieceSource fills every byte with a deterministic pattern and counts
// how many pieces were fetched from the network.
type countingPieceSource struct {
	mu      sync.Mutex
	fetches int
}

func (s *countingPieceSource) ReadAtContext(ctx context.Context, dst []byte, off, _ int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	s.fetches++
	s.mu.Unlock()
	for i := range dst {
		dst[i] = byte((off + int64(i)) % 251)
	}
	return len(dst), nil
}

func (s *countingPieceSource) Close() error { return nil }

func (s *countingPieceSource) fetched() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetches
}

// TestCachedPieceReadHitsCacheOnce pins the read path to a single cache lookup
// per span: the first read populates the cache, the second one must be served
// from it with exactly one hit and no extra network fetch.
func TestCachedPieceReadHitsCacheOnce(t *testing.T) {
	const pieceLength = int64(64)
	store := cache.New(1 << 20)
	coordinator := newTestCoordinator(store)
	file := playbackTestFile(2*pieceLength, pieceLength, store, coordinator)
	file.torrentKey = coordinator.torrentKey
	source := &countingPieceSource{}
	file.loader = source

	want := make([]byte, pieceLength)
	for i := range want {
		want[i] = byte(int64(i) % 251)
	}

	first := make([]byte, pieceLength)
	if _, err := file.ReadAtContext(context.Background(), first, 0); err != nil {
		t.Fatalf("priming read: %v", err)
	}
	if got := source.fetched(); got != 1 {
		t.Fatalf("priming read fetched %d pieces, want 1", got)
	}

	before := store.Stats()

	second := make([]byte, pieceLength)
	if _, err := file.ReadAtContext(context.Background(), second, 0); err != nil {
		t.Fatalf("cached read: %v", err)
	}

	after := store.Stats()
	if got := after.Hits - before.Hits; got != 1 {
		t.Fatalf("cached span produced %d cache hits, want 1", got)
	}
	if got := after.Misses - before.Misses; got != 0 {
		t.Fatalf("cached span produced %d cache misses, want 0", got)
	}
	if got := source.fetched(); got != 1 {
		t.Fatalf("cached read fetched %d extra pieces, want 0", got-1)
	}
	if string(second) != string(want) {
		t.Fatalf("cached read returned wrong bytes")
	}
}
