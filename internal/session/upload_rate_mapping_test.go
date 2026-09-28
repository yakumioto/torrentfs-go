package session_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"golang.org/x/time/rate"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

func uploadRateTorrentsDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "torrents")
}

// TestUploadRateUnconfiguredLeavesClientLimiterUntouched pins the compatibility
// contract: with no upload configuration the session must not replace or mutate
// the client's shared unlimited limiter.
func TestUploadRateUnconfiguredLeavesClientLimiterUntouched(t *testing.T) {
	var observed *rate.Limiter
	sess := newLoopbackSessionWithCustomize(t, testConfig(), uploadRateTorrentsDir(t), func(cc *session.TorrentClientConfig) {
		observed = cc.UploadRateLimiter
	})

	if observed == nil {
		t.Fatal("client config has no upload limiter")
	}
	if observed != torrent.NewDefaultClientConfig().UploadRateLimiter {
		t.Fatal("session replaced the client's shared default upload limiter")
	}
	limit, managed := sess.UploadRateLimitForTest()
	if managed || limit != rate.Inf {
		t.Fatalf("upload limit = %v managed = %t, want the unlimited default", limit, managed)
	}
	if sess.UploadRateScheduledForTest() {
		t.Fatal("unconfigured session started an upload-rate scheduler")
	}
}

// TestUploadRateAloneUsesAllDayLimit checks that a rate without a schedule
// installs a private limiter and starts no boundary scheduler.
func TestUploadRateAloneUsesAllDayLimit(t *testing.T) {
	cfg := testConfig()
	cfg.Upload.RateLimitBytesPerSecond = 65536

	var observed *rate.Limiter
	sess := newLoopbackSessionWithCustomize(t, cfg, uploadRateTorrentsDir(t), func(cc *session.TorrentClientConfig) {
		observed = cc.UploadRateLimiter
	})

	if observed == nil || observed == torrent.NewDefaultClientConfig().UploadRateLimiter {
		t.Fatal("session did not install a private upload limiter")
	}
	limit, managed := sess.UploadRateLimitForTest()
	if !managed || limit != rate.Limit(65536) {
		t.Fatalf("upload limit = %v managed = %t, want 65536", limit, managed)
	}
	if sess.UploadRateScheduledForTest() {
		t.Fatal("all-day rate started a boundary scheduler")
	}
}

// TestUploadRateScheduleResolvesEachBoundary drives the configured schedule
// through the same code path a window boundary uses and pins both exact edges.
func TestUploadRateScheduleResolvesEachBoundary(t *testing.T) {
	cfg := testConfig()
	cfg.Upload.RateLimitBytesPerSecond = 65536
	cfg.Upload.Schedule = config.UploadSchedule{Start: "08:00", End: "22:00"}

	sess := newLoopbackSession(t, cfg, uploadRateTorrentsDir(t))
	if !sess.UploadRateScheduledForTest() {
		t.Fatal("scheduled configuration did not start a boundary scheduler")
	}

	at := func(hour, minute int) time.Time {
		return time.Date(2026, 3, 1, hour, minute, 0, 0, time.Local)
	}
	tests := []struct {
		name string
		when time.Time
		want rate.Limit
	}{
		{name: "one minute before start", when: at(7, 59), want: rate.Inf},
		{name: "start minute", when: at(8, 0), want: rate.Limit(65536)},
		{name: "one minute before end", when: at(21, 59), want: rate.Limit(65536)},
		{name: "end minute", when: at(22, 0), want: rate.Inf},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, managed := sess.ApplyUploadRatePolicyForTest(tt.when)
			if !managed {
				t.Fatal("session has no managed upload limiter")
			}
			if got != tt.want {
				t.Fatalf("limit at %s = %v, want %v", tt.when.Format("15:04"), got, tt.want)
			}
		})
	}
}
