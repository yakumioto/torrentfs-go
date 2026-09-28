package config_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

func TestDefaultDisablesUploadLimiting(t *testing.T) {
	cfg := config.Default()
	if cfg.Upload.RateLimitBytesPerSecond != 0 {
		t.Fatalf("Default upload rate = %d, want 0", cfg.Upload.RateLimitBytesPerSecond)
	}
	if cfg.Upload.Schedule != (config.UploadSchedule{}) {
		t.Fatalf("Default upload schedule = %+v, want empty", cfg.Upload.Schedule)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate: %v", err)
	}
}

func TestParseUploadScheduleTime(t *testing.T) {
	tests := []struct {
		raw  string
		want int
		ok   bool
	}{
		{raw: "00:00", want: 0, ok: true},
		{raw: "08:00", want: 480, ok: true},
		{raw: "22:00", want: 1320, ok: true},
		{raw: "23:59", want: 1439, ok: true},
		{raw: ""},
		{raw: "8:00"},
		{raw: "08:0"},
		{raw: "0800"},
		{raw: " 08:00"},
		{raw: "08:00 "},
		{raw: "24:00"},
		{raw: "08:60"},
		{raw: "-1:00"},
		{raw: "08:00:00"},
		{raw: "aa:bb"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := config.ParseUploadScheduleTime(tt.raw)
			if !tt.ok {
				if err == nil {
					t.Fatalf("ParseUploadScheduleTime(%q) = %d, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseUploadScheduleTime(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("ParseUploadScheduleTime(%q) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

func TestLoadReadsUploadSection(t *testing.T) {
	path := writeConfig(t, `
[upload]
rate_limit_bytes_per_second = 1048576

[upload.schedule]
start = "08:00"
end = "22:00"
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Upload.RateLimitBytesPerSecond != 1048576 {
		t.Fatalf("upload rate = %d, want 1048576", cfg.Upload.RateLimitBytesPerSecond)
	}
	if cfg.Upload.Schedule != (config.UploadSchedule{Start: "08:00", End: "22:00"}) {
		t.Fatalf("upload schedule = %+v, want 08:00-22:00", cfg.Upload.Schedule)
	}
}

func TestLoadReadsAllDayUploadRate(t *testing.T) {
	path := writeConfig(t, "[upload]\nrate_limit_bytes_per_second = 65536\n")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Upload.RateLimitBytesPerSecond != 65536 || cfg.Upload.Schedule != (config.UploadSchedule{}) {
		t.Fatalf("upload = %+v, want an all-day 65536 B/s limit", cfg.Upload)
	}
}

func TestLoadRejectsInvalidUpload(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		field string
	}{
		{
			name:  "negative rate",
			body:  "[upload]\nrate_limit_bytes_per_second = -1\n",
			field: "upload.rate_limit_bytes_per_second",
		},
		{
			name:  "schedule without a rate",
			body:  "[upload.schedule]\nstart = \"08:00\"\nend = \"22:00\"\n",
			field: "upload.rate_limit_bytes_per_second",
		},
		{
			name:  "missing end",
			body:  "[upload]\nrate_limit_bytes_per_second = 1024\n\n[upload.schedule]\nstart = \"08:00\"\n",
			field: "upload.schedule.end",
		},
		{
			name:  "missing start",
			body:  "[upload]\nrate_limit_bytes_per_second = 1024\n\n[upload.schedule]\nend = \"22:00\"\n",
			field: "upload.schedule.start",
		},
		{
			name:  "malformed start",
			body:  "[upload]\nrate_limit_bytes_per_second = 1024\n\n[upload.schedule]\nstart = \"8:00\"\nend = \"22:00\"\n",
			field: "upload.schedule.start",
		},
		{
			name:  "out-of-range end",
			body:  "[upload]\nrate_limit_bytes_per_second = 1024\n\n[upload.schedule]\nstart = \"08:00\"\nend = \"24:00\"\n",
			field: "upload.schedule.end",
		},
		{
			name:  "start equals end",
			body:  "[upload]\nrate_limit_bytes_per_second = 1024\n\n[upload.schedule]\nstart = \"08:00\"\nend = \"08:00\"\n",
			field: "upload.schedule.end",
		},
		{
			name:  "start after end",
			body:  "[upload]\nrate_limit_bytes_per_second = 1024\n\n[upload.schedule]\nstart = \"22:00\"\nend = \"08:00\"\n",
			field: "upload.schedule.end",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(writeConfig(t, tt.body))
			if err == nil {
				t.Fatal("Load accepted an invalid upload configuration")
			}
			if !errors.Is(err, config.ErrInvalid) {
				t.Fatalf("Load error = %v, want ErrInvalid", err)
			}
			var validationErr *config.ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("Load error = %T %v, want ValidationError", err, err)
			}
			if validationErr.Field != tt.field {
				t.Fatalf("ValidationError.Field = %q, want %q", validationErr.Field, tt.field)
			}
		})
	}
}

func TestLoadUploadEnvironmentOverridesAndClearsFile(t *testing.T) {
	path := writeConfig(t, `
[upload]
rate_limit_bytes_per_second = 1024

[upload.schedule]
start = "08:00"
end = "22:00"
`)
	t.Setenv("TORRENTFS_UPLOAD_RATE_LIMIT_BYTES_PER_SECOND", "2048")
	t.Setenv("TORRENTFS_UPLOAD_SCHEDULE_START", "")
	t.Setenv("TORRENTFS_UPLOAD_SCHEDULE_END", "")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Upload.RateLimitBytesPerSecond != 2048 {
		t.Fatalf("upload rate = %d, want environment value 2048", cfg.Upload.RateLimitBytesPerSecond)
	}
	if cfg.Upload.Schedule != (config.UploadSchedule{}) {
		t.Fatalf("upload schedule = %+v, want the pair cleared", cfg.Upload.Schedule)
	}
}

func TestLoadUploadEnvironmentParsesEachField(t *testing.T) {
	t.Setenv("TORRENTFS_UPLOAD_RATE_LIMIT_BYTES_PER_SECOND", "4096")
	t.Setenv("TORRENTFS_UPLOAD_SCHEDULE_START", "06:30")
	t.Setenv("TORRENTFS_UPLOAD_SCHEDULE_END", "18:30")

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Upload.RateLimitBytesPerSecond != 4096 {
		t.Fatalf("upload rate = %d, want 4096", cfg.Upload.RateLimitBytesPerSecond)
	}
	if cfg.Upload.Schedule != (config.UploadSchedule{Start: "06:30", End: "18:30"}) {
		t.Fatalf("upload schedule = %+v, want 06:30-18:30", cfg.Upload.Schedule)
	}
}

func TestLoadUploadEnvironmentRejectsInvalidCombination(t *testing.T) {
	t.Setenv("TORRENTFS_UPLOAD_SCHEDULE_START", "08:00")
	t.Setenv("TORRENTFS_UPLOAD_SCHEDULE_END", "22:00")

	_, err := config.Load("")
	if err == nil {
		t.Fatal("Load accepted a schedule without a rate")
	}
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "upload.rate_limit_bytes_per_second" {
		t.Fatalf("Load error = %v, want upload.rate_limit_bytes_per_second", err)
	}
}

// TestShippedConfigurationsKeepUploadDisabled pins the documented default for
// the files a user copies: both ship the upload section disabled so an omitted
// override never silently throttles uploads.
func TestShippedConfigurationsKeepUploadDisabled(t *testing.T) {
	for _, name := range []string{"torrentfs.example.toml", "docker/torrentfs.toml"} {
		path := filepath.Join("..", "..", name)
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("Load(%s): %v", name, err)
		}
		if cfg.Upload.RateLimitBytesPerSecond != 0 || cfg.Upload.Schedule != (config.UploadSchedule{}) {
			t.Fatalf("%s enables upload limiting by default: %+v", name, cfg.Upload)
		}
	}
}
