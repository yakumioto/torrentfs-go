package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Upload rate settings are per-torrents-directory state, not static
// configuration: the Web UI writes them through the settings API and a restart
// restores them before the BitTorrent client is created.
var (
	// ErrInvalidUploadRateSettings reports a settings value that violates the
	// documented contract. The wrapped cause names the offending field.
	ErrInvalidUploadRateSettings = errors.New("session: invalid upload rate settings")
	// ErrUploadRateSettingsStorageUnavailable reports that the settings were not
	// published: the previous file and the running limit are unchanged.
	ErrUploadRateSettingsStorageUnavailable = errors.New("session: upload rate settings storage unavailable")
	// ErrUploadRateSettingsDurabilityUnconfirmed reports that the new settings
	// are published and applied, but the directory could not be synced, so a
	// restart is not guaranteed to observe them.
	ErrUploadRateSettingsDurabilityUnconfirmed = errors.New("session: upload rate settings durability unconfirmed")
)

const (
	uploadRateSettingsFileName = "upload_rate.json"
	uploadRateSettingsVersion  = 1
)

// UploadRateSettings is the session-wide aggregate peer upload limit and the
// optional daily window in which it applies.
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

// uploadRateSettingsRecord is the on-disk schema. It is deliberately separate
// from the API type so the version field never becomes part of the public
// contract.
type uploadRateSettingsRecord struct {
	Version                 int                 `json:"version"`
	RateLimitBytesPerSecond int64               `json:"rate_limit_bytes_per_second"`
	Schedule                *UploadRateSchedule `json:"schedule"`
}

func cloneUploadRateSettings(settings UploadRateSettings) UploadRateSettings {
	clone := settings
	if settings.Schedule != nil {
		schedule := *settings.Schedule
		clone.Schedule = &schedule
	}
	return clone
}

// uploadRateSettingsPath derives the sidecar path from the session's metadata
// directory. No caller supplies a path, so the API cannot redirect the write.
func uploadRateSettingsPath(metadataDir string) string {
	return filepath.Join(metadataDir, uploadRateSettingsFileName)
}

// parseUploadScheduleTime parses a strict 24-hour HH:MM boundary into minutes
// after midnight. Only the exact five-character form is accepted, so "8:00",
// "08:60" and "24:00" are all rejected rather than silently normalized.
func parseUploadScheduleTime(raw string) (int, error) {
	if len(raw) != 5 || raw[2] != ':' ||
		raw[0] < '0' || raw[0] > '9' || raw[1] < '0' || raw[1] > '9' ||
		raw[3] < '0' || raw[3] > '9' || raw[4] < '0' || raw[4] > '9' {
		return 0, errors.New("must use 24-hour HH:MM")
	}
	hour := int(raw[0]-'0')*10 + int(raw[1]-'0')
	minute := int(raw[3]-'0')*10 + int(raw[4]-'0')
	if hour > 23 || minute > 59 {
		return 0, errors.New("must use 24-hour HH:MM")
	}
	return hour*60 + minute, nil
}

// validateUploadRateSettings normalizes and validates settings and compiles the
// runtime policy from the same pass, so validation and compilation cannot
// drift. The API and the sidecar loader both go through it.
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
	start, err := parseUploadScheduleTime(settings.Schedule.Start)
	if err != nil {
		return fail("schedule.start: %v", err)
	}
	end, err := parseUploadScheduleTime(settings.Schedule.End)
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

// loadUploadRateSettings reads the sidecar for a metadata directory. A missing
// file is the upgrade path from a deployment that predates the feature and means
// "unlimited"; anything else that cannot be read or understood fails startup so
// an operator never believes a limit is in force when it is not.
func loadUploadRateSettings(metadataDir string) (UploadRateSettings, uploadRatePolicy, error) {
	path := uploadRateSettingsPath(metadataDir)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		settings, policy, _ := validateUploadRateSettings(UploadRateSettings{})
		return settings, policy, nil
	} else if err != nil {
		return UploadRateSettings{}, uploadRatePolicy{}, fmt.Errorf("session: inspect %s: %w", path, err)
	}
	if _, err := requireRegularFile(path); err != nil {
		return UploadRateSettings{}, uploadRatePolicy{}, fmt.Errorf("session: %s: %w", uploadRateSettingsFileName, err)
	}
	file, err := os.Open(path)
	if err != nil {
		return UploadRateSettings{}, uploadRatePolicy{}, fmt.Errorf("session: read %s: %w", uploadRateSettingsFileName, err)
	}
	defer func() { _ = file.Close() }()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var record uploadRateSettingsRecord
	if err := decoder.Decode(&record); err != nil {
		return UploadRateSettings{}, uploadRatePolicy{}, fmt.Errorf("session: decode %s: %w", uploadRateSettingsFileName, err)
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return UploadRateSettings{}, uploadRatePolicy{}, fmt.Errorf("session: decode %s: unexpected trailing data", uploadRateSettingsFileName)
	}
	if record.Version != uploadRateSettingsVersion {
		return UploadRateSettings{}, uploadRatePolicy{}, fmt.Errorf("session: %s: unsupported version %d", uploadRateSettingsFileName, record.Version)
	}

	settings, policy, err := validateUploadRateSettings(UploadRateSettings{
		RateLimitBytesPerSecond: record.RateLimitBytesPerSecond,
		Schedule:                record.Schedule,
	})
	if err != nil {
		return UploadRateSettings{}, uploadRatePolicy{}, fmt.Errorf("session: %s: %w", uploadRateSettingsFileName, err)
	}
	return settings, policy, nil
}

// saveUploadRateSettings atomically publishes settings. The returned published
// flag reports whether the rename committed: once it did, the file is visible to
// a restart even when the error says the directory sync failed, so the caller
// must continue applying the new value instead of rolling back.
func saveUploadRateSettings(metadataDir string, settings UploadRateSettings) (bool, error) {
	path := uploadRateSettingsPath(metadataDir)
	if _, err := os.Lstat(path); err == nil {
		if _, err := requireRegularFile(path); err != nil {
			return false, fmt.Errorf("%w: %v", ErrUploadRateSettingsStorageUnavailable, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("%w: %v", ErrUploadRateSettingsStorageUnavailable, err)
	}

	normalized, _, err := validateUploadRateSettings(settings)
	if err != nil {
		return false, err
	}
	data, err := json.Marshal(uploadRateSettingsRecord{
		Version:                 uploadRateSettingsVersion,
		RateLimitBytesPerSecond: normalized.RateLimitBytesPerSecond,
		Schedule:                normalized.Schedule,
	})
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrUploadRateSettingsStorageUnavailable, err)
	}

	published, err := writeFileAtomicPublished(path, append(data, '\n'))
	if err != nil {
		if published {
			return true, fmt.Errorf("%w: %v", ErrUploadRateSettingsDurabilityUnconfirmed, err)
		}
		return false, fmt.Errorf("%w: %v", ErrUploadRateSettingsStorageUnavailable, err)
	}
	return true, nil
}

// UploadRateSettings returns the settings in force for this session.
func (s *Session) UploadRateSettings() UploadRateSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneUploadRateSettings(s.uploadRateSettings)
}

// SetUploadRateSettings validates, persists, and applies settings and returns the
// normalized value.
//
// The atomic rename is the commit point. A failure before it leaves the file,
// the stored settings, and the running limit untouched. A failure after it still
// applies the new value, because a restart will read the published file, and
// reports ErrUploadRateSettingsDurabilityUnconfirmed rather than pretending the
// save failed.
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
	// A request canceled while it waited for the lock must not start a write.
	if err := ctx.Err(); err != nil {
		return UploadRateSettings{}, err
	}

	published, saveErr := saveUploadRateSettings(s.metadataDir, normalized)
	if !published {
		s.logger.Error("upload rate settings save failed",
			"rate_limit_bytes_per_second", normalized.RateLimitBytesPerSecond, "err", saveErr)
		return UploadRateSettings{}, saveErr
	}

	// The file is visible to a restart, so memory and the limiter follow it even
	// when the directory sync failed.
	s.uploadRateSettings = normalized
	s.uploadRate.setPolicy(policy, time.Now())
	if saveErr != nil {
		s.logger.Error("upload rate settings applied without confirmed durability",
			"rate_limit_bytes_per_second", normalized.RateLimitBytesPerSecond, "err", saveErr)
		return normalized, saveErr
	}
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
