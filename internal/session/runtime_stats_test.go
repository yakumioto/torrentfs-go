package session

import (
	"fmt"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/cache"
)

func TestRuntimeStatsViewUsesUsefulDownloadAndDataUploadCounters(t *testing.T) {
	startedAt := time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC)
	var connStats torrent.ConnStats
	connStats.BytesRead.Add(100)
	connStats.BytesReadData.Add(200)
	connStats.BytesReadUsefulData.Add(300)
	connStats.BytesWritten.Add(400)
	connStats.BytesWrittenData.Add(500)

	counters := cache.Stats{Hits: 11, Misses: 2, Evictions: 3, EvictedBytes: 40, PutRejections: 4, PinReservationRejections: 5}
	got := runtimeStatsView(startedAt, 600, 700, connStats, counters)
	if !got.StartedAt.Equal(startedAt) {
		t.Fatalf("StartedAt = %v, want %v", got.StartedAt, startedAt)
	}
	if got.CacheUsedBytes != 600 || got.CacheCapacityBytes != 700 {
		t.Fatalf("cache = (%d, %d), want (600, 700)", got.CacheUsedBytes, got.CacheCapacityBytes)
	}
	if got.DownloadedBytes != 300 {
		t.Fatalf("DownloadedBytes = %d, want useful data 300", got.DownloadedBytes)
	}
	if got.UploadedBytes != 500 {
		t.Fatalf("UploadedBytes = %d, want data 500", got.UploadedBytes)
	}
	if got.CacheHits != 11 || got.CacheMisses != 2 || got.CacheEvictions != 3 ||
		got.CacheEvictedBytes != 40 || got.CachePutRejections != 4 || got.CachePinReservationRejections != 5 {
		t.Fatalf("cache counters = %+v, want the cache snapshot", got)
	}
}

func TestPrefetchTotalsSumEveryRegisteredTorrent(t *testing.T) {
	newCoordinator := func(pieces int) *prefetchCoordinator {
		c := newTestCoordinator(cache.New(1 << 20))
		c.torrentKey = fmt.Sprintf("torrent-%d", pieces)
		c.hasAnchor = true
		c.anchor = prefetchAnchor{fileStart: 0, fileSize: 1000, pieceLength: 100, torrentSize: 1000}
		for i := 0; i < pieces; i++ {
			c.cache.Put(c.key(i), make([]byte, 100))
			c.active[i] = struct{}{}
		}
		c.updateBufferedLocked()
		return c
	}

	s := &Session{torrents: map[metainfo.Hash]*Torrent{}}
	for i, coordinator := range []*prefetchCoordinator{newCoordinator(2), newCoordinator(3)} {
		var hash metainfo.Hash
		hash[0] = byte(i + 1)
		s.torrents[hash] = &Torrent{coordinator: coordinator}
	}
	// A torrent whose metadata has not arrived yet has no coordinator and must
	// not be dropped from the total by accident, only contribute nothing.
	var pending metainfo.Hash
	pending[0] = 9
	s.torrents[pending] = &Torrent{}

	got := s.prefetchTotals()
	if want := int64(500); got.bufferedBytes != want {
		t.Fatalf("bufferedBytes = %d, want %d", got.bufferedBytes, want)
	}
	if want := 5; got.activePieces != want {
		t.Fatalf("activePieces = %d, want %d", got.activePieces, want)
	}
	if want := int64(500); got.activeBytes != want {
		t.Fatalf("activeBytes = %d, want %d", got.activeBytes, want)
	}
}

// TestPrefetchTotalsReadsCoordinatorUnderTorrentLock keeps -race honest: the
// coordinator pointer is written under the torrent lock while stats are polled.
func TestPrefetchTotalsReadsCoordinatorUnderTorrentLock(t *testing.T) {
	var hash metainfo.Hash
	torrent := &Torrent{}
	s := &Session{torrents: map[metainfo.Hash]*Torrent{hash: torrent}}
	coordinator := newTestCoordinator(cache.New(1 << 20))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			torrent.mu.Lock()
			torrent.coordinator = coordinator
			torrent.mu.Unlock()
			torrent.mu.Lock()
			torrent.coordinator = nil
			torrent.mu.Unlock()
		}
	}()
	for i := 0; i < 200; i++ {
		_ = s.prefetchTotals()
	}
	<-done
}
