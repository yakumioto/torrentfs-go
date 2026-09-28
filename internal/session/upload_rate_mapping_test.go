package session_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent"
	"golang.org/x/time/rate"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

func uploadRateTorrentsDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "torrents")
}

// settingsFileExists reports whether the settings sidecar was published.
func settingsFileExists(t *testing.T, torrentsDir string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(torrentsDir, ".metadata", "upload_rate.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatalf("stat settings sidecar: %v", err)
	}
	return true
}

// TestUploadRateUnconfiguredInstallsPrivateLimiter pins the compatibility
// contract of the settings-driven design: every session allocates its own
// limiter (so the UI can enable limiting later without replacing the pointer the
// client already holds), the client's shared default limiter is never adopted,
// and no sidecar is created before the first save.
func TestUploadRateUnconfiguredInstallsPrivateLimiter(t *testing.T) {
	torrentsDir := uploadRateTorrentsDir(t)
	var observed *rate.Limiter
	sess := newLoopbackSessionWithCustomize(t, testConfig(), torrentsDir, func(cc *session.TorrentClientConfig) {
		observed = cc.UploadRateLimiter
	})

	if observed == nil {
		t.Fatal("client config has no upload limiter")
	}
	if observed == torrent.NewDefaultClientConfig().UploadRateLimiter {
		t.Fatal("session adopted the client's shared default upload limiter")
	}
	limit, installed := sess.UploadRateLimitForTest()
	if installed != observed {
		t.Fatal("session limiter is not the one installed on the client config")
	}
	if limit != rate.Inf {
		t.Fatalf("unconfigured limit = %v, want rate.Inf", limit)
	}
	if got := sess.UploadRateSettings(); got.RateLimitBytesPerSecond != 0 || got.Schedule != nil {
		t.Fatalf("unconfigured settings = %+v, want disabled", got)
	}
	if settingsFileExists(t, torrentsDir) {
		t.Fatal("an unconfigured session created a settings sidecar")
	}
}

// TestUploadRateSessionsDoNotShareLimiter proves the private limiter prevents
// one session's limit from leaking into another in the same process.
func TestUploadRateSessionsDoNotShareLimiter(t *testing.T) {
	first := newLoopbackSession(t, testConfig(), uploadRateTorrentsDir(t))
	second := newLoopbackSession(t, testConfig(), uploadRateTorrentsDir(t))

	_, firstLimiter := first.UploadRateLimitForTest()
	_, secondLimiter := second.UploadRateLimitForTest()
	if firstLimiter == secondLimiter {
		t.Fatal("two sessions share one limiter")
	}

	if _, err := first.SetUploadRateSettings(context.Background(), session.UploadRateSettings{
		RateLimitBytesPerSecond: 65536,
	}); err != nil {
		t.Fatalf("SetUploadRateSettings: %v", err)
	}
	if limit, _ := first.UploadRateLimitForTest(); limit != rate.Limit(65536) {
		t.Fatalf("first limit = %v, want 65536", limit)
	}
	if limit, _ := second.UploadRateLimitForTest(); limit != rate.Inf {
		t.Fatalf("second limit = %v, want the untouched %v", limit, rate.Inf)
	}
}

// TestUploadRateSettingsApplyImmediately covers the three documented shapes
// through the real settings path.
func TestUploadRateSettingsApplyImmediately(t *testing.T) {
	ctx := context.Background()
	torrentsDir := uploadRateTorrentsDir(t)
	sess := newLoopbackSession(t, testConfig(), torrentsDir)

	tests := []struct {
		name     string
		settings session.UploadRateSettings
		want     rate.Limit
	}{
		{name: "disabled", settings: session.UploadRateSettings{}, want: rate.Inf},
		{
			name:     "all day",
			settings: session.UploadRateSettings{RateLimitBytesPerSecond: 131072},
			want:     rate.Limit(131072),
		},
		{
			name: "window that contains the running clock",
			settings: session.UploadRateSettings{
				RateLimitBytesPerSecond: 262144,
				Schedule:                &session.UploadRateSchedule{Start: "00:00", End: "23:59"},
			},
			want: rate.Limit(262144),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applied, err := sess.SetUploadRateSettings(ctx, tt.settings)
			if err != nil {
				t.Fatalf("SetUploadRateSettings: %v", err)
			}
			if !sameSettings(applied, tt.settings) {
				t.Fatalf("applied settings = %+v, want %+v", applied, tt.settings)
			}
			if limit, _ := sess.UploadRateLimitForTest(); limit != tt.want {
				t.Fatalf("limit = %v, want %v", limit, tt.want)
			}
			stored := sess.UploadRateSettings()
			if !sameSettings(stored, tt.settings) {
				t.Fatalf("stored settings = %+v, want %+v", stored, tt.settings)
			}
			if !settingsFileExists(t, torrentsDir) {
				t.Fatal("save did not publish the settings sidecar")
			}
		})
	}
}

func sameSettings(a, b session.UploadRateSettings) bool {
	if a.RateLimitBytesPerSecond != b.RateLimitBytesPerSecond {
		return false
	}
	switch {
	case a.Schedule == nil && b.Schedule == nil:
		return true
	case a.Schedule == nil || b.Schedule == nil:
		return false
	default:
		return *a.Schedule == *b.Schedule
	}
}

// TestUploadRateSettingsRejectInvalid covers the validation contract at the
// session boundary, including that a rejected save changes nothing.
func TestUploadRateSettingsRejectInvalid(t *testing.T) {
	ctx := context.Background()
	torrentsDir := uploadRateTorrentsDir(t)
	sess := newLoopbackSession(t, testConfig(), torrentsDir)
	if _, err := sess.SetUploadRateSettings(ctx, session.UploadRateSettings{
		RateLimitBytesPerSecond: 4096,
	}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	tests := []struct {
		name     string
		settings session.UploadRateSettings
	}{
		{name: "negative rate", settings: session.UploadRateSettings{RateLimitBytesPerSecond: -1}},
		{
			name: "schedule without a rate",
			settings: session.UploadRateSettings{
				Schedule: &session.UploadRateSchedule{Start: "08:00", End: "22:00"},
			},
		},
		{
			name: "schedule with one side missing",
			settings: session.UploadRateSettings{
				RateLimitBytesPerSecond: 1024,
				Schedule:                &session.UploadRateSchedule{Start: "08:00"},
			},
		},
		{
			name: "schedule with a loose time",
			settings: session.UploadRateSettings{
				RateLimitBytesPerSecond: 1024,
				Schedule:                &session.UploadRateSchedule{Start: "8:00", End: "22:00"},
			},
		},
		{
			name: "schedule in reverse",
			settings: session.UploadRateSettings{
				RateLimitBytesPerSecond: 1024,
				Schedule:                &session.UploadRateSchedule{Start: "22:00", End: "08:00"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := sess.SetUploadRateSettings(ctx, tt.settings); !errors.Is(err, session.ErrInvalidUploadRateSettings) {
				t.Fatalf("SetUploadRateSettings error = %v, want ErrInvalidUploadRateSettings", err)
			}
			if limit, _ := sess.UploadRateLimitForTest(); limit != rate.Limit(4096) {
				t.Fatalf("limit after a rejected save = %v, want the previous 4096", limit)
			}
			if got := sess.UploadRateSettings(); got.RateLimitBytesPerSecond != 4096 {
				t.Fatalf("stored settings after a rejected save = %+v, want the previous value", got)
			}
		})
	}
}

// TestUploadRateSettingsSurviveRestart proves the sidecar is loaded before the
// client exists: a fresh session on the same torrents directory restores the
// saved limit without any API call.
func TestUploadRateSettingsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	torrentsDir := uploadRateTorrentsDir(t)

	first := newLivingSession(t, testConfig(), torrentsDir, nil)
	if _, err := first.SetUploadRateSettings(ctx, session.UploadRateSettings{
		RateLimitBytesPerSecond: 32768,
		Schedule:                &session.UploadRateSchedule{Start: "00:00", End: "23:59"},
	}); err != nil {
		t.Fatalf("SetUploadRateSettings: %v", err)
	}
	closeSession(t, first)

	restarted := newLivingSession(t, testConfig(), torrentsDir, nil)
	defer closeSession(t, restarted)

	restored := restarted.UploadRateSettings()
	if restored.RateLimitBytesPerSecond != 32768 || restored.Schedule == nil ||
		restored.Schedule.Start != "00:00" || restored.Schedule.End != "23:59" {
		t.Fatalf("restored settings = %+v, want the saved window", restored)
	}
	if limit, _ := restarted.UploadRateLimitForTest(); limit != rate.Limit(32768) {
		t.Fatalf("restored limit = %v, want 32768", limit)
	}
}

// TestUploadRateSettingsDefaultToUnlimitedWithoutSidecar pins the upgrade path.
func TestUploadRateSettingsDefaultToUnlimitedWithoutSidecar(t *testing.T) {
	torrentsDir := uploadRateTorrentsDir(t)
	sess := newLoopbackSession(t, testConfig(), torrentsDir)

	if got := sess.UploadRateSettings(); got.RateLimitBytesPerSecond != 0 || got.Schedule != nil {
		t.Fatalf("settings without a sidecar = %+v, want disabled", got)
	}
	if limit, _ := sess.UploadRateLimitForTest(); limit != rate.Inf {
		t.Fatalf("limit without a sidecar = %v, want rate.Inf", limit)
	}
}
