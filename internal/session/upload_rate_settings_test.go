package session

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/time/rate"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

// TestNormalizeUploadRateSettings covers the runtime normalizer used by the
// configuration mapping and by the retired-sidecar migration: it accepts the
// documented shapes and reports every unusable one.
func TestNormalizeUploadRateSettings(t *testing.T) {
	valid := []UploadRateSettings{
		{},
		{RateLimitBytesPerSecond: 1024},
		{
			RateLimitBytesPerSecond: 1024,
			Schedule:                &UploadRateSchedule{Start: "08:00", End: "22:00"},
		},
	}
	for _, settings := range valid {
		if _, err := NormalizeUploadRateSettings(settings); err != nil {
			t.Errorf("NormalizeUploadRateSettings(%+v) = %v, want accepted", settings, err)
		}
	}

	invalid := []UploadRateSettings{
		{RateLimitBytesPerSecond: -1},
		{Schedule: &UploadRateSchedule{Start: "08:00", End: "22:00"}},
		{RateLimitBytesPerSecond: 1024, Schedule: &UploadRateSchedule{Start: "08:00"}},
		{RateLimitBytesPerSecond: 1024, Schedule: &UploadRateSchedule{Start: "8:00", End: "22:00"}},
		{RateLimitBytesPerSecond: 1024, Schedule: &UploadRateSchedule{Start: "22:00", End: "08:00"}},
	}
	for _, settings := range invalid {
		if _, err := NormalizeUploadRateSettings(settings); !errors.Is(err, ErrInvalidUploadRateSettings) {
			t.Errorf("NormalizeUploadRateSettings(%+v) = %v, want ErrInvalidUploadRateSettings", settings, err)
		}
	}
}

// TestSetUploadRateSettingsConcurrentUpdatesAndClose drives the runtime update
// path from several goroutines while the session closes, and then checks the
// last applied settings still match the limiter. The session lock serializes
// every update, so memory and the limiter cannot diverge.
func TestSetUploadRateSettingsConcurrentUpdatesAndClose(t *testing.T) {
	torrentsDir := filepath.Join(t.TempDir(), "torrents")
	if err := ensureDirectory(torrentsDir); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	sess, err := New(config.Default(), torrentsDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const (
		writers   = 3
		perWriter = 12
	)
	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		failures []error
	)
	for writer := 0; writer < writers; writer++ {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			for attempt := 0; attempt < perWriter; attempt++ {
				_, err := sess.SetUploadRateSettings(context.Background(), UploadRateSettings{
					RateLimitBytesPerSecond: int64(1024 * (writer + 1) * (attempt + 1)),
				})
				if err != nil && !errors.Is(err, filesystem.ErrClosed) {
					errMu.Lock()
					failures = append(failures, err)
					errMu.Unlock()
					return
				}
			}
		}(writer)
	}

	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()
	if len(failures) > 0 {
		t.Fatalf("concurrent updates failed: %v", failures)
	}

	if _, err := sess.SetUploadRateSettings(context.Background(), UploadRateSettings{
		RateLimitBytesPerSecond: 4096,
	}); !errors.Is(err, filesystem.ErrClosed) {
		t.Fatalf("setter after Close = %v, want ErrClosed", err)
	}

	stored := sess.UploadRateSettings()
	if stored.RateLimitBytesPerSecond < 0 || stored.Schedule != nil {
		t.Fatalf("stored settings after concurrent updates = %+v, want one all-day limit", stored)
	}
	want := rate.Inf
	if stored.RateLimitBytesPerSecond > 0 {
		want = rate.Limit(stored.RateLimitBytesPerSecond)
	}
	if limit, _ := sess.UploadRateLimitForTest(); limit != want {
		t.Fatalf("running limit %v does not match the stored rate %d", limit, stored.RateLimitBytesPerSecond)
	}
}
