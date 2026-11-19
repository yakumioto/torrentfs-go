package session

import (
	"time"

	"github.com/anacrolix/torrent"

	"github.com/yakumioto/torrentfs-go/internal/cache"
)

// RuntimeStatsView is the session-wide cache and transfer snapshot.
type RuntimeStatsView struct {
	StartedAt          time.Time
	CacheUsedBytes     int64
	CacheCapacityBytes int64
	DownloadedBytes    int64
	UploadedBytes      int64

	// Cache counters come straight from the piece cache. Hits and misses count
	// lookups, so they measure the read path rather than network traffic.
	CacheHits                     uint64
	CacheMisses                   uint64
	CacheEvictions                uint64
	CacheEvictedBytes             int64
	CachePutRejections            uint64
	CachePinReservationRejections uint64

	// Prefetch holds the session-wide aggregate of per-torrent prefetch
	// snapshots. Only monotone or instantaneous gauges are exposed; counters the
	// coordinator never increments stay out of the payload.
	PrefetchBufferedBytes int64
	PrefetchActivePieces  int
	PrefetchActiveBytes   int64
}

// RuntimeStats returns the current session-wide cache and transfer counters.
func (s *Session) RuntimeStats() RuntimeStatsView {
	connStats := s.cl.ConnStats()
	cacheStats := s.pieceCache.Stats()
	prefetch := s.prefetchTotals()
	view := runtimeStatsView(
		s.startedAt,
		s.pieceCache.Size(),
		s.pieceCache.Capacity(),
		connStats,
		cacheStats,
	)
	view.PrefetchBufferedBytes = prefetch.bufferedBytes
	view.PrefetchActivePieces = prefetch.activePieces
	view.PrefetchActiveBytes = prefetch.activeBytes
	return view
}

type prefetchTotals struct {
	bufferedBytes int64
	activePieces  int
	activeBytes   int64
}

// prefetchTotals sums the prefetch gauges of every registered torrent. A
// torrent without a coordinator (metadata still pending) contributes nothing.
func (s *Session) prefetchTotals() prefetchTotals {
	s.mu.RLock()
	torrents := make([]*Torrent, 0, len(s.torrents))
	for _, t := range s.torrents {
		torrents = append(torrents, t)
	}
	s.mu.RUnlock()

	// The session lock only guards the registry. Each coordinator pointer is
	// owned by its torrent and written under that torrent's lock, so it is read
	// per torrent instead of nesting a registry lock around every entry.
	coordinators := make([]*prefetchCoordinator, 0, len(torrents))
	for _, t := range torrents {
		t.mu.Lock()
		coordinator := t.coordinator
		t.mu.Unlock()
		if coordinator != nil {
			coordinators = append(coordinators, coordinator)
		}
	}

	var totals prefetchTotals
	for _, coordinator := range coordinators {
		snapshot := coordinator.snapshot()
		totals.bufferedBytes += snapshot.BufferedBytes
		totals.activePieces += snapshot.ActivePieces
		totals.activeBytes += snapshot.ActiveBytes
	}
	return totals
}

func runtimeStatsView(startedAt time.Time, cacheUsedBytes, cacheCapacityBytes int64, connStats torrent.ConnStats, counters cache.Stats) RuntimeStatsView {
	return RuntimeStatsView{
		StartedAt:                     startedAt,
		CacheUsedBytes:                cacheUsedBytes,
		CacheCapacityBytes:            cacheCapacityBytes,
		DownloadedBytes:               connStats.BytesReadUsefulData.Int64(),
		UploadedBytes:                 connStats.BytesWrittenData.Int64(),
		CacheHits:                     counters.Hits,
		CacheMisses:                   counters.Misses,
		CacheEvictions:                counters.Evictions,
		CacheEvictedBytes:             counters.EvictedBytes,
		CachePutRejections:            counters.PutRejections,
		CachePinReservationRejections: counters.PinReservationRejections,
	}
}
