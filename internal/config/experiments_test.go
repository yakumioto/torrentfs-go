package config_test

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// TestUploadExperimentLoadsFromTheConfigurationFile covers the contract that
// the upload statistics experiment lives under [experiments.upload] like every
// other setting: the configuration file names it and the values reach the
// configuration.
func TestUploadExperimentLoadsFromTheConfigurationFile(t *testing.T) {
	path := writeConfig(t, "[experiments.upload]\nmode = \"inject\"\nrate = \"2MiB/s\"\njitter = \"30\"\ncap = \"8MiB\"\nseed = 42\nannounce_log = \"/tmp/announce.jsonl\"\npeer_sample_interval = \"10s\"\n")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	upload := cfg.Experiments.Upload
	if upload.Mode != "inject" {
		t.Errorf("experiments.upload.mode = %q, want inject", upload.Mode)
	}
	if upload.Rate != config.ByteRate(2<<20) {
		t.Errorf("experiments.upload.rate = %v, want %v", upload.Rate, config.ByteRate(2<<20))
	}
	if upload.Jitter != config.Percent(30) {
		t.Errorf("experiments.upload.jitter = %v, want 30", upload.Jitter)
	}
	if upload.Cap.Int64() != 8<<20 {
		t.Errorf("experiments.upload.cap = %d, want %d", upload.Cap.Int64(), 8<<20)
	}
	if upload.Seed != 42 {
		t.Errorf("experiments.upload.seed = %d, want 42", upload.Seed)
	}
	if upload.AnnounceLog != "/tmp/announce.jsonl" {
		t.Errorf("experiments.upload.announce_log = %q, want /tmp/announce.jsonl", upload.AnnounceLog)
	}
	if upload.PeerSampleInterval.String() != "10s" {
		t.Errorf("experiments.upload.peer_sample_interval = %q, want 10s", upload.PeerSampleInterval)
	}
}

// TestUploadExperimentDefaultsToBaseline covers the contract that the
// experiment is always on: an unconfigured experiment reports the real
// counters in the baseline arm instead of being disabled.
func TestUploadExperimentDefaultsToBaseline(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, "[log]\nlevel = \"debug\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Experiments.Upload.Mode != "baseline" {
		t.Errorf("experiments.upload.mode = %q, want the baseline default", cfg.Experiments.Upload.Mode)
	}
	if cfg.Experiments.Upload.Jitter != config.Percent(20) {
		t.Errorf("experiments.upload.jitter = %v, want the 20%% default", cfg.Experiments.Upload.Jitter)
	}
}

// TestUploadExperimentRejectsAnEmptyMode covers the contract that a mode is
// always named: an empty value is not the missing mode of an old
// configuration, it is a value the user wrote and the schema rejects.
func TestUploadExperimentRejectsAnEmptyMode(t *testing.T) {
	_, err := config.Load(writeConfig(t, "[experiments.upload]\nmode = \"\"\n"))
	if err == nil {
		t.Fatal("Load accepted an empty mode")
	}
	if !strings.Contains(err.Error(), "experiments.upload.mode") {
		t.Errorf("Load error %q does not name experiments.upload.mode", err)
	}
}

// TestUploadExperimentRejectsAnUnknownMode covers the contract that a mistyped
// mode is reported instead of silently ignored.
func TestUploadExperimentRejectsAnUnknownMode(t *testing.T) {
	_, err := config.Load(writeConfig(t, "[experiments.upload]\nmode = \"nonsense\"\n"))
	if err == nil {
		t.Fatal("Load succeeded")
	}
	if !strings.Contains(err.Error(), "experiments.upload.mode") {
		t.Errorf("Load error %q does not name experiments.upload.mode", err)
	}
}

// TestUploadExperimentRejectsAnOutOfRangeJitter covers the contract that a
// jitter outside 0-100 percent is reported.
func TestUploadExperimentRejectsAnOutOfRangeJitter(t *testing.T) {
	_, err := config.Load(writeConfig(t, "[experiments.upload]\njitter = \"150\"\n"))
	if err == nil {
		t.Fatal("Load succeeded")
	}
	if !strings.Contains(err.Error(), "experiments.upload.jitter") {
		t.Errorf("Load error %q does not name experiments.upload.jitter", err)
	}
}

// TestUploadExperimentRejectsAMultiplierBelowOne covers the contract that a
// multiplier of one or less is reported: it would report the real counters.
func TestUploadExperimentRejectsAMultiplierBelowOne(t *testing.T) {
	_, err := config.Load(writeConfig(t, "[experiments.upload]\nmultiplier = \"0.5\"\n"))
	if err == nil {
		t.Fatal("Load succeeded")
	}
	if !strings.Contains(err.Error(), "experiments.upload.multiplier") {
		t.Errorf("Load error %q does not name experiments.upload.multiplier", err)
	}
}

// TestUploadExperimentRejectsAMalformedValue covers the contract that a
// malformed value is reported with the configuration key it belongs to.
func TestUploadExperimentRejectsAMalformedValue(t *testing.T) {
	_, err := config.Load(writeConfig(t, "[experiments.upload]\nrate = \"not-a-rate\"\n"))
	if err == nil {
		t.Fatal("Load succeeded")
	}
	if !strings.Contains(err.Error(), "experiments.upload.rate") {
		t.Errorf("Load error %q does not name experiments.upload.rate", err)
	}
}

// TestUploadExperimentComesFromTheEnvironment covers the contract that the
// environment variable of a key derives from its new path.
func TestUploadExperimentComesFromTheEnvironment(t *testing.T) {
	lookup := testEnvironment(map[string]string{
		"TORRENTFS_EXPERIMENTS_UPLOAD_MODE":   "amplify",
		"TORRENTFS_EXPERIMENTS_UPLOAD_RATE":   "3MiB/s",
		"TORRENTFS_EXPERIMENTS_UPLOAD_JITTER": "15",
	})

	cfg, err := config.LoadWithEnvironment("", lookup)
	if err != nil {
		t.Fatalf("LoadWithEnvironment: %v", err)
	}
	upload := cfg.Experiments.Upload
	if upload.Mode != "amplify" {
		t.Errorf("experiments.upload.mode = %q, want amplify", upload.Mode)
	}
	if upload.Rate != config.ByteRate(3<<20) {
		t.Errorf("experiments.upload.rate = %v, want %v", upload.Rate, config.ByteRate(3<<20))
	}
	if upload.Jitter != config.Percent(15) {
		t.Errorf("experiments.upload.jitter = %v, want 15", upload.Jitter)
	}
}

// TestUploadExperimentRejectsAnUnknownEnvironmentMode covers the contract that
// a mistyped environment value is reported with the configuration key and the
// environment variable.
func TestUploadExperimentRejectsAnUnknownEnvironmentMode(t *testing.T) {
	lookup := testEnvironment(map[string]string{
		"TORRENTFS_EXPERIMENTS_UPLOAD_MODE": "nonsense",
	})

	_, err := config.LoadWithEnvironment("", lookup)
	if err == nil {
		t.Fatal("LoadWithEnvironment succeeded")
	}
	if !strings.Contains(err.Error(), "experiments.upload.mode") {
		t.Errorf("Load error %q does not name experiments.upload.mode", err)
	}
	if !strings.Contains(err.Error(), "TORRENTFS_EXPERIMENTS_UPLOAD_MODE") {
		t.Errorf("Load error %q does not name the environment variable", err)
	}
}

// TestUploadExperimentIsValidatedEvenWhenUnset covers the contract that the
// shape of a value is checked wherever it stands: a mistyped value is a
// mistake even when no mode is configured.
func TestUploadExperimentIsValidatedEvenWhenUnset(t *testing.T) {
	cfg := config.Default()
	cfg.Experiments.Upload.Jitter = config.Percent(150)

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate succeeded")
	}
	var validationErrs *config.ValidationErrors
	if !errors.As(err, &validationErrs) {
		t.Fatalf("Validate error = %T, want *config.ValidationErrors", err)
	}
	if !validationErrs.HasField("experiments.upload.jitter") {
		t.Errorf("Validate did not report experiments.upload.jitter: %v", err)
	}
}

// TestRemovedExperimentKeysAreReportedTogether covers the contract that a
// document still using the flat [lab] section learns about every replacement
// at once.
func TestRemovedExperimentKeysAreReportedTogether(t *testing.T) {
	_, err := config.Load(writeConfig(t, "[lab]\nupload_mode = \"inject\"\nupload_rate = \"1MiB/s\"\n"))
	if err == nil {
		t.Fatal("Load accepted the removed [lab] keys")
	}
	for _, want := range []string{
		"lab.upload_mode", "experiments.upload.mode",
		"lab.upload_rate", "experiments.upload.rate",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load error %q does not name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), `removed TOML key "lab"`) {
		t.Errorf("Load error %q names the section instead of the keys it holds", err)
	}
}

// TestRemovedExperimentEnvironmentNamesJoinOtherProblems covers the contract
// that the removed environment variables are reported with the other startup
// problems instead of stopping the report.
func TestRemovedExperimentEnvironmentNamesJoinOtherProblems(t *testing.T) {
	_, err := config.LoadWithEnvironment("", testEnvironment(map[string]string{
		"TORRENTFS_LAB_UPLOAD_MODE": "inject",
		"TORRENTFS_LAB_UPLOAD_RATE": "1MiB/s",
		"TORRENTFS_CACHE_CAPACITY":  "0B",
	}))
	if err == nil {
		t.Fatal("Load accepted the removed environment variables")
	}
	for _, want := range []string{
		"TORRENTFS_LAB_UPLOAD_MODE", "TORRENTFS_EXPERIMENTS_UPLOAD_MODE",
		"TORRENTFS_LAB_UPLOAD_RATE", "TORRENTFS_EXPERIMENTS_UPLOAD_RATE",
		"cache.capacity",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load error %q does not name %q", err, want)
		}
	}
}

// TestConfigPrintUsesTheNewExperimentKeys covers the contract that the audit
// report names the configuration keys the program reads.
func TestConfigPrintUsesTheNewExperimentKeys(t *testing.T) {
	printed, err := config.Print(writeConfig(t, "[experiments.upload]\nmode = \"inject\"\nrate = \"1MiB/s\"\n"), config.PrintOptions{})
	if err != nil {
		t.Fatalf("Print: %v", err)
	}
	values := make(map[string]string, len(printed.Keys))
	for _, key := range printed.Keys {
		values[key.Path] = key.Value
		if strings.HasPrefix(key.Path, "lab.") {
			t.Errorf("the printed configuration still names the removed key %q", key.Path)
		}
	}
	if got := values["experiments.upload.mode"]; got != "inject" {
		t.Errorf("experiments.upload.mode = %q, want inject", got)
	}
	if got := values["experiments.upload.rate"]; got != "1048576" {
		t.Errorf("experiments.upload.rate = %q, want the byte rate 1048576", got)
	}
}

// TestStartupLogUsesTheNewExperimentKeys covers the contract that the
// effective-configuration log names the configuration keys the program reads.
func TestStartupLogUsesTheNewExperimentKeys(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	logger.Info("effective configuration", config.Summary(config.Default(), config.Sources{}))
	record := output.String()
	if !strings.Contains(record, "experiments.upload.mode") {
		t.Errorf("the effective configuration log does not name experiments.upload.mode: %s", record)
	}
	if strings.Contains(record, "lab.upload_mode") {
		t.Errorf("the effective configuration log still names lab.upload_mode: %s", record)
	}
}
