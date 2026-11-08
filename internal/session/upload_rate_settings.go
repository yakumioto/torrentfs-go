package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// ErrInvalidUploadRateSettings reports a settings value that violates the
// documented contract. The wrapped cause names the offending field.
var ErrInvalidUploadRateSettings = errors.New("session: invalid upload rate settings")

// UploadRateSettings is the session-wide aggregate peer upload limit and the
// optional daily window in which it applies.
//
// The value is runtime state: the configuration schema owns persistence, and
// the startup and hot-apply paths both derive this value from the
// configuration.
type UploadRateSettings struct {
	// RateLimitBytesPerSecond is the aggregate payload upload rate across every
	// torrent and peer in the session. Zero disables upload limiting.
	RateLimitBytesPerSecond int64 `json:"rate_limit_bytes_per_second"`
	// Schedule is nil when the limit applies around the clock.
	Schedule *UploadRateSchedule `json:"schedule"`
}

// UploadRateSchedule is one same-day window, half-open in server-local time.
type UploadRateSchedule struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

func cloneUploadRateSettings(settings UploadRateSettings) UploadRateSettings {
	clone := settings
	if settings.Schedule != nil {
		schedule := *settings.Schedule
		clone.Schedule = &schedule
	}
	return clone
}

// validateUploadRateSettings normalizes and validates settings and compiles the
// runtime policy from the same pass, so validation and compilation cannot
// drift.
func validateUploadRateSettings(settings UploadRateSettings) (UploadRateSettings, uploadRatePolicy, error) {
	fail := func(format string, args ...any) (UploadRateSettings, uploadRatePolicy, error) {
		return UploadRateSettings{}, uploadRatePolicy{}, fmt.Errorf("%w: %s", ErrInvalidUploadRateSettings, fmt.Sprintf(format, args...))
	}
	if settings.RateLimitBytesPerSecond < 0 {
		return fail("rate_limit_bytes_per_second must not be negative")
	}

	normalized := UploadRateSettings{RateLimitBytesPerSecond: settings.RateLimitBytesPerSecond}
	if settings.Schedule == nil {
		return normalized, compileUploadRatePolicy(normalized), nil
	}
	if settings.Schedule.Start == "" || settings.Schedule.End == "" {
		return fail("schedule requires both start and end")
	}
	if settings.RateLimitBytesPerSecond <= 0 {
		return fail("schedule requires a positive rate_limit_bytes_per_second")
	}
	start, err := config.ParseUploadRateTime(settings.Schedule.Start)
	if err != nil {
		return fail("schedule.start: %v", err)
	}
	end, err := config.ParseUploadRateTime(settings.Schedule.End)
	if err != nil {
		return fail("schedule.end: %v", err)
	}
	if start >= end {
		return fail("schedule.start must be before schedule.end")
	}
	schedule := *settings.Schedule
	normalized.Schedule = &schedule
	return normalized, compileUploadRatePolicy(normalized), nil
}

// NormalizeUploadRateSettings validates and normalizes runtime upload rate
// settings.
//
// The configuration mapping, the hot apply and the retired-sidecar migration
// all go through this function, so a value one of them accepts means the same
// thing to the others.
func NormalizeUploadRateSettings(settings UploadRateSettings) (UploadRateSettings, error) {
	normalized, _, err := validateUploadRateSettings(settings)
	return normalized, err
}

// UploadRateSettings returns the settings in force for this session.
func (s *Session) UploadRateSettings() UploadRateSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneUploadRateSettings(s.uploadRateSettings)
}

// SetUploadRateSettings validates and applies settings and returns the
// normalized value.
//
// The call updates only the running session: persistence belongs to the
// configuration file, and the configuration controller hot-applies a saved
// value through ApplyUploadRateConfig. A rejected value leaves the stored
// settings and the running limit untouched.
func (s *Session) SetUploadRateSettings(ctx context.Context, settings UploadRateSettings) (UploadRateSettings, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return UploadRateSettings{}, err
	}
	normalized, policy, err := validateUploadRateSettings(settings)
	if err != nil {
		return UploadRateSettings{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureActiveLocked(); err != nil {
		return UploadRateSettings{}, err
	}
	// A request canceled while it waited for the lock must not apply anything.
	if err := ctx.Err(); err != nil {
		return UploadRateSettings{}, err
	}

	s.uploadRateSettings = normalized
	s.uploadRate.setPolicy(policy, time.Now())
	s.logger.Info("upload rate settings updated",
		"rate_limit_bytes_per_second", normalized.RateLimitBytesPerSecond,
		"schedule_start", scheduleStartLabel(normalized),
		"schedule_end", scheduleEndLabel(normalized),
		"limited", s.uploadRate.limitedNow(),
	)
	return normalized, nil
}

func scheduleStartLabel(settings UploadRateSettings) string {
	if settings.Schedule == nil {
		return ""
	}
	return settings.Schedule.Start
}

func scheduleEndLabel(settings UploadRateSettings) string {
	if settings.Schedule == nil {
		return ""
	}
	return settings.Schedule.End
}
