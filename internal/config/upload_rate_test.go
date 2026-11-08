package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// TestUploadRateLoadsFromTheConfigurationFile covers the contract that the
// upload limit and its daily window are configuration keys: the file names
// them and the values reach the configuration.
func TestUploadRateLoadsFromTheConfigurationFile(t *testing.T) {
	path := writeConfig(t, `[upload_rate]
limit = "1MiB/s"
schedule_start = "08:00"
schedule_end = "22:00"
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	uploadRate := cfg.UploadRate
	if uploadRate.Limit != config.ByteRate(1<<20) {
		t.Errorf("upload_rate.limit = %v, want %v", uploadRate.Limit, config.ByteRate(1<<20))
	}
	if uploadRate.ScheduleStart != "08:00" {
		t.Errorf("upload_rate.schedule_start = %q, want 08:00", uploadRate.ScheduleStart)
	}
	if uploadRate.ScheduleEnd != "22:00" {
		t.Errorf("upload_rate.schedule_end = %q, want 22:00", uploadRate.ScheduleEnd)
	}
}

// TestUploadRateIsUnlimitedAndAllDayByDefault covers the contract that an
// unset upload limit does not limit anything and applies around the clock.
func TestUploadRateIsUnlimitedAndAllDayByDefault(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, "[log]\nlevel = \"debug\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UploadRate != (config.UploadRate{}) {
		t.Errorf("upload_rate is configured without being set: %+v", cfg.UploadRate)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the default upload_rate does not validate: %v", err)
	}
}

// TestUploadRateComesFromTheEnvironment covers the contract that the
// environment variables derive mechanically from the configuration paths.
func TestUploadRateComesFromTheEnvironment(t *testing.T) {
	lookup := testEnvironment(map[string]string{
		"TORRENTFS_UPLOAD_RATE_LIMIT":          "2MiB/s",
		"TORRENTFS_UPLOAD_RATE_SCHEDULE_START": "01:30",
		"TORRENTFS_UPLOAD_RATE_SCHEDULE_END":   "05:30",
	})

	cfg, err := config.LoadWithEnvironment("", lookup)
	if err != nil {
		t.Fatalf("LoadWithEnvironment: %v", err)
	}
	uploadRate := cfg.UploadRate
	if uploadRate.Limit != config.ByteRate(2<<20) {
		t.Errorf("upload_rate.limit = %v, want %v", uploadRate.Limit, config.ByteRate(2<<20))
	}
	if uploadRate.ScheduleStart != "01:30" || uploadRate.ScheduleEnd != "05:30" {
		t.Errorf("upload_rate window = %q-%q, want 01:30-05:30", uploadRate.ScheduleStart, uploadRate.ScheduleEnd)
	}
}

// TestUploadRateLimitAcceptsTheEmptyValue covers the "empty means unlimited"
// contract on both inputs of the configuration page: the environment and the
// text form a save sends.
func TestUploadRateLimitAcceptsTheEmptyValue(t *testing.T) {
	cfg, err := config.LoadWithEnvironment("", testEnvironment(map[string]string{
		"TORRENTFS_UPLOAD_RATE_LIMIT": "",
	}))
	if err != nil {
		t.Fatalf("LoadWithEnvironment: %v", err)
	}
	if cfg.UploadRate.Limit != 0 {
		t.Errorf("upload_rate.limit = %v, want the empty value", cfg.UploadRate.Limit)
	}

	candidate := config.Default()
	candidate.UploadRate.Limit = 1024
	if err := config.SetText(&candidate, "upload_rate.limit", ""); err != nil {
		t.Fatalf("SetText empty limit: %v", err)
	}
	if candidate.UploadRate.Limit != 0 {
		t.Errorf("the empty text form left upload_rate.limit = %v, want 0", candidate.UploadRate.Limit)
	}
}

// TestUploadRateValidationRejectsUnusableConfigurations covers the
// cross-field contract shared by the start and the configuration page: a
// window needs a positive limit, both boundaries, strict 24-hour HH:MM, and a
// start before its end on the same day.
func TestUploadRateValidationRejectsUnusableConfigurations(t *testing.T) {
	tests := []struct {
		name      string
		upload    config.UploadRate
		wantField string
		wantCause string
	}{
		{
			name:      "negative limit",
			upload:    config.UploadRate{Limit: config.ByteRate(-1)},
			wantField: "upload_rate.limit",
			wantCause: "must not be negative",
		},
		{
			name:      "start without end",
			upload:    config.UploadRate{Limit: 1024, ScheduleStart: "08:00"},
			wantField: "upload_rate.schedule_end",
			wantCause: "together",
		},
		{
			name:      "end without start",
			upload:    config.UploadRate{Limit: 1024, ScheduleEnd: "22:00"},
			wantField: "upload_rate.schedule_start",
			wantCause: "together",
		},
		{
			name: "window without a positive limit",
			upload: config.UploadRate{
				ScheduleStart: "08:00",
				ScheduleEnd:   "22:00",
			},
			wantField: "upload_rate.limit",
			wantCause: "positive",
		},
		{
			name: "loose start time",
			upload: config.UploadRate{
				Limit:         1024,
				ScheduleStart: "8:00",
				ScheduleEnd:   "22:00",
			},
			wantField: "upload_rate.schedule_start",
			wantCause: "HH:MM",
		},
		{
			name: "out-of-range end time",
			upload: config.UploadRate{
				Limit:         1024,
				ScheduleStart: "08:00",
				ScheduleEnd:   "24:00",
			},
			wantField: "upload_rate.schedule_end",
			wantCause: "HH:MM",
		},
		{
			name: "start not before end",
			upload: config.UploadRate{
				Limit:         1024,
				ScheduleStart: "22:00",
				ScheduleEnd:   "22:00",
			},
			wantField: "upload_rate.schedule_start",
			wantCause: "before",
		},
		{
			name: "window that crosses midnight",
			upload: config.UploadRate{
				Limit:         1024,
				ScheduleStart: "22:00",
				ScheduleEnd:   "08:00",
			},
			wantField: "upload_rate.schedule_start",
			wantCause: "before",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.UploadRate = tt.upload

			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate succeeded")
			}
			if !errors.Is(err, config.ErrInvalid) {
				t.Fatalf("Validate error = %v, want config.ErrInvalid", err)
			}
			var validationErrs *config.ValidationErrors
			if !errors.As(err, &validationErrs) {
				t.Fatalf("Validate error = %T, want *config.ValidationErrors", err)
			}
			if !validationErrs.HasField(tt.wantField) {
				t.Errorf("Validate did not report %q: %v", tt.wantField, err)
			}
			if !strings.Contains(err.Error(), tt.wantCause) {
				t.Errorf("Validate error %q does not say %q", err, tt.wantCause)
			}
		})
	}
}

// TestUploadRateSchemaMetadata covers the contract that the configuration page
// derives the section from the schema: all three keys are hot-applied by the
// session and carry their mechanically derived environment variable.
func TestUploadRateSchemaMetadata(t *testing.T) {
	want := map[string]string{
		"upload_rate.limit":          "TORRENTFS_UPLOAD_RATE_LIMIT",
		"upload_rate.schedule_start": "TORRENTFS_UPLOAD_RATE_SCHEDULE_START",
		"upload_rate.schedule_end":   "TORRENTFS_UPLOAD_RATE_SCHEDULE_END",
	}
	found := map[string]bool{}
	for _, key := range config.Schema() {
		environment, ok := want[key.Path]
		if !ok {
			continue
		}
		found[key.Path] = true
		if key.Environment != environment {
			t.Errorf("the environment of %q = %q, want %q", key.Path, key.Environment, environment)
		}
		if key.Component != "session" {
			t.Errorf("the component of %q = %q, want session", key.Path, key.Component)
		}
		if !key.HotApply {
			t.Errorf("the configuration key %q is not hot-applied", key.Path)
		}
		if key.Kind != "byte rate" && key.Kind != "string" {
			t.Errorf("the kind of %q = %q, want byte rate or string", key.Path, key.Kind)
		}
	}
	for path := range want {
		if !found[path] {
			t.Errorf("the schema has no configuration key %q", path)
		}
	}
}
