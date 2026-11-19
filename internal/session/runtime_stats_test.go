package session

import (
	"testing"
	"time"

	"github.com/anacrolix/torrent"

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
