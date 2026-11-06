package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// TestLabSettingsLoadFromTheConfigurationFile covers the contract that the
// upload statistics experiment is configured like every other setting: the
// configuration file names it and the values reach the configuration.
func TestLabSettingsLoadFromTheConfigurationFile(t *testing.T) {
	path := writeConfig(t, "[lab]\nupload_mode = \"inject\"\nupload_rate = \"2MiB/s\"\nupload_jitter = \"30\"\nupload_cap = \"8MiB\"\nupload_seed = 42\nannounce_log = \"/tmp/lab.jsonl\"\npeer_sample_interval = \"10s\"\n")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Lab.UploadMode != "inject" {
		t.Errorf("lab.upload_mode = %q, want inject", cfg.Lab.UploadMode)
	}
	if cfg.Lab.UploadRate != config.ByteRate(2<<20) {
		t.Errorf("lab.upload_rate = %v, want %v", cfg.Lab.UploadRate, config.ByteRate(2<<20))
	}
	if cfg.Lab.UploadJitter != config.Percent(30) {
		t.Errorf("lab.upload_jitter = %v, want 30", cfg.Lab.UploadJitter)
	}
	if cfg.Lab.UploadCap.Int64() != 8<<20 {
		t.Errorf("lab.upload_cap = %d, want %d", cfg.Lab.UploadCap.Int64(), 8<<20)
	}
	if cfg.Lab.UploadSeed != 42 {
		t.Errorf("lab.upload_seed = %d, want 42", cfg.Lab.UploadSeed)
	}
	if cfg.Lab.AnnounceLog != "/tmp/lab.jsonl" {
		t.Errorf("lab.announce_log = %q, want /tmp/lab.jsonl", cfg.Lab.AnnounceLog)
	}
	if cfg.Lab.PeerSampleInterval.String() != "10s" {
		t.Errorf("lab.peer_sample_interval = %q, want 10s", cfg.Lab.PeerSampleInterval)
	}
}

// TestLabSettingsAreOffByDefault covers the contract that the experiment does
// nothing unless it is configured.
func TestLabSettingsAreOffByDefault(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, "[log]\nlevel = \"debug\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Lab.UploadMode != "" || cfg.Lab.UploadRate != 0 || cfg.Lab.AnnounceLog != "" {
		t.Errorf("the experiment is configured without being set: %+v", cfg.Lab)
	}
}

// TestLabSettingsRejectAnUnknownMode covers the contract that a mistyped mode
// is reported instead of silently ignored.
func TestLabSettingsRejectAnUnknownMode(t *testing.T) {
	_, err := config.Load(writeConfig(t, "[lab]\nupload_mode = \"nonsense\"\n"))
	if err == nil {
		t.Fatal("Load succeeded")
	}
	if !strings.Contains(err.Error(), "lab.upload_mode") {
		t.Errorf("Load error %q does not name lab.upload_mode", err)
	}
}

// TestLabSettingsRejectAnOutOfRangeJitter covers the contract that a jitter
// outside 0-100 percent is reported.
func TestLabSettingsRejectAnOutOfRangeJitter(t *testing.T) {
	_, err := config.Load(writeConfig(t, "[lab]\nupload_jitter = \"150\"\n"))
	if err == nil {
		t.Fatal("Load succeeded")
	}
	if !strings.Contains(err.Error(), "lab.upload_jitter") {
		t.Errorf("Load error %q does not name lab.upload_jitter", err)
	}
}

// TestLabSettingsRejectAMultiplierBelowOne covers the contract that a
// multiplier of one or less is reported: it would report the real counters.
func TestLabSettingsRejectAMultiplierBelowOne(t *testing.T) {
	_, err := config.Load(writeConfig(t, "[lab]\nupload_multiplier = \"0.5\"\n"))
	if err == nil {
		t.Fatal("Load succeeded")
	}
	if !strings.Contains(err.Error(), "lab.upload_multiplier") {
		t.Errorf("Load error %q does not name lab.upload_multiplier", err)
	}
}

// TestLabSettingsRejectAMalformedValue covers the contract that a malformed
// value is reported with the configuration key it belongs to.
func TestLabSettingsRejectAMalformedValue(t *testing.T) {
	_, err := config.Load(writeConfig(t, "[lab]\nupload_rate = \"not-a-rate\"\n"))
	if err == nil {
		t.Fatal("Load succeeded")
	}
	if !strings.Contains(err.Error(), "lab.upload_rate") {
		t.Errorf("Load error %q does not name lab.upload_rate", err)
	}
}

// TestLabSettingsComeFromTheEnvironment covers the contract that the
// environment configures the experiment under the names it always used.
func TestLabSettingsComeFromTheEnvironment(t *testing.T) {
	lookup := testEnvironment(map[string]string{
		"TORRENTFS_LAB_UPLOAD_MODE":   "amplify",
		"TORRENTFS_LAB_UPLOAD_RATE":   "3MiB/s",
		"TORRENTFS_LAB_UPLOAD_JITTER": "15",
	})

	cfg, err := config.LoadWithEnvironment("", lookup)
	if err != nil {
		t.Fatalf("LoadWithEnvironment: %v", err)
	}
	if cfg.Lab.UploadMode != "amplify" {
		t.Errorf("lab.upload_mode = %q, want amplify", cfg.Lab.UploadMode)
	}
	if cfg.Lab.UploadRate != config.ByteRate(3<<20) {
		t.Errorf("lab.upload_rate = %v, want %v", cfg.Lab.UploadRate, config.ByteRate(3<<20))
	}
	if cfg.Lab.UploadJitter != config.Percent(15) {
		t.Errorf("lab.upload_jitter = %v, want 15", cfg.Lab.UploadJitter)
	}
}

// TestLabSettingsRejectAnUnknownEnvironmentMode covers the contract that a
// mistyped environment value is reported with the configuration key.
func TestLabSettingsRejectAnUnknownEnvironmentMode(t *testing.T) {
	lookup := testEnvironment(map[string]string{
		"TORRENTFS_LAB_UPLOAD_MODE": "nonsense",
	})

	_, err := config.LoadWithEnvironment("", lookup)
	if err == nil {
		t.Fatal("LoadWithEnvironment succeeded")
	}
	if !strings.Contains(err.Error(), "lab.upload_mode") {
		t.Errorf("Load error %q does not name lab.upload_mode", err)
	}
	if !strings.Contains(err.Error(), "TORRENTFS_LAB_UPLOAD_MODE") {
		t.Errorf("Load error %q does not name the environment variable", err)
	}
}

// TestLabSettingsAreValidatedEvenWhenTheExperimentIsOff covers the contract
// that the shape of a value is checked wherever it stands: a mistyped value
// is a mistake even when no mode is configured.
func TestLabSettingsAreValidatedEvenWhenTheExperimentIsOff(t *testing.T) {
	cfg := config.Default()
	cfg.Lab.UploadJitter = config.Percent(150)

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate succeeded")
	}
	var validationErrs *config.ValidationErrors
	if !errors.As(err, &validationErrs) {
		t.Fatalf("Validate error = %T, want *config.ValidationErrors", err)
	}
	if !validationErrs.HasField("lab.upload_jitter") {
		t.Errorf("Validate did not report lab.upload_jitter: %v", err)
	}
}
