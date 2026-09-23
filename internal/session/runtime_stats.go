package session

import (
	"time"

	"github.com/anacrolix/torrent"
)

// RuntimeStatsView is the session-wide cache and transfer snapshot.
type RuntimeStatsView struct {
	StartedAt          time.Time
	CacheUsedBytes     int64
	CacheCapacityBytes int64
	DownloadedBytes    int64
	UploadedBytes      int64
}

// RuntimeStats returns the current session-wide cache and transfer counters.
func (s *Session) RuntimeStats() RuntimeStatsView {
	connStats := s.cl.ConnStats()
	return runtimeStatsView(
		s.startedAt,
		s.pieceCache.Size(),
		s.pieceCache.Capacity(),
		connStats,
	)
}

func runtimeStatsView(startedAt time.Time, cacheUsedBytes, cacheCapacityBytes int64, connStats torrent.ConnStats) RuntimeStatsView {
	return RuntimeStatsView{
		StartedAt:          startedAt,
		CacheUsedBytes:     cacheUsedBytes,
		CacheCapacityBytes: cacheCapacityBytes,
		DownloadedBytes:    connStats.BytesReadUsefulData.Int64(),
		UploadedBytes:      connStats.BytesWrittenData.Int64(),
	}
}
