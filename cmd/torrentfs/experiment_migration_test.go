package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/experiments"
)

// emptyEnvironment is an environment with no variable set. The migration tests
// reload the migrated file layer without the process configuration.
type emptyEnvironment struct{}

// Lookup reports that no environment variable is set.
func (emptyEnvironment) Lookup(string) (string, bool) { return "", false }

// Names reports no environment variable.
func (emptyEnvironment) Names() []string { return nil }

// writeLegacyExperimentSidecar writes the retired .metadata/lab_settings.json
// record for a migration test.
func writeLegacyExperimentSidecar(t *testing.T, torrentsDir, body string) string {
	t.Helper()
	metadataDir := filepath.Join(torrentsDir, ".metadata")
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("make metadata dir: %v", err)
	}
	path := filepath.Join(metadataDir, "lab_settings.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write legacy sidecar: %v", err)
	}
	return path
}

func migrationLogger(buffer *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestMigrateLegacyExperimentSettingsMergesIntoTheConfigurationFile covers the
// writable upgrade path: the sidecar values fill the keys the file does not
// name, the file keeps the values it does name, and the sidecar is archived.
func TestMigrateLegacyExperimentSettingsMergesIntoTheConfigurationFile(t *testing.T) {
	work := t.TempDir()
	torrentsDir := filepath.Join(work, "torrents")
	configPath := filepath.Join(work, "torrentfs.toml")
	document := `[log]
level = "debug"

[experiments.upload]
jitter = "30"
`
	if err := os.WriteFile(configPath, []byte(document), 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}
	sidecar := writeLegacyExperimentSidecar(t, torrentsDir, `{"version":1,"mode":"inject","rate_bytes_per_second":8388608,"multiplier":0,"jitter_fraction":0.05,"cap_bytes":1048576}`)

	var logs bytes.Buffer
	result, err := migrateLegacyExperimentSettings(configPath, torrentsDir, migrationLogger(&logs))
	if err != nil {
		t.Fatalf("migrateLegacyExperimentSettings: %v", err)
	}
	if !result.Migrated {
		t.Fatalf("migration did not report the merge; logs:\n%s", logs.String())
	}
	if result.Runtime != nil {
		t.Fatalf("migration reports a runtime fallback %+v although it merged", *result.Runtime)
	}
	if result.ArchivedPath != sidecar+".migrated" {
		t.Fatalf("archived path = %q, want %q", result.ArchivedPath, sidecar+".migrated")
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Fatalf("the sidecar was not archived: %v", err)
	}
	archived, err := os.ReadFile(result.ArchivedPath)
	if err != nil {
		t.Fatalf("read archived sidecar: %v", err)
	}
	if !strings.Contains(string(archived), `"mode":"inject"`) {
		t.Fatalf("archived sidecar = %s", archived)
	}

	loaded, err := config.LoadWithEnvironment(configPath, emptyEnvironment{})
	if err != nil {
		t.Fatalf("reload migrated configuration: %v", err)
	}
	upload := loaded.Experiments.Upload
	if upload.Mode != "inject" {
		t.Errorf("experiments.upload.mode = %q, want inject", upload.Mode)
	}
	if upload.Rate != config.ByteRate(8<<20) {
		t.Errorf("experiments.upload.rate = %v, want 8MiB/s", upload.Rate)
	}
	if upload.Multiplier != 0 {
		t.Errorf("experiments.upload.multiplier = %v, want the unused zero", upload.Multiplier)
	}
	if upload.Cap.Int64() != 1<<20 {
		t.Errorf("experiments.upload.cap = %d, want 1MiB", upload.Cap.Int64())
	}
	if upload.Jitter != config.Percent(30) {
		t.Errorf("experiments.upload.jitter = %v, want the file value 30", upload.Jitter)
	}
	if loaded.Log.Level != "debug" {
		t.Errorf("log.level = %q, want the untouched file value", loaded.Log.Level)
	}
}

// TestMigrateLegacyExperimentSettingsKeepsAZeroJitter covers the value that
// used to mean "no deviation": the runtime default is 20 percent, so the
// migration has to carry an explicit zero through instead of letting the
// configuration default replace it.
func TestMigrateLegacyExperimentSettingsKeepsAZeroJitter(t *testing.T) {
	work := t.TempDir()
	torrentsDir := filepath.Join(work, "torrents")
	configPath := filepath.Join(work, "torrentfs.toml")
	if err := os.WriteFile(configPath, []byte("[log]\nlevel = \"debug\"\n"), 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}
	writeLegacyExperimentSidecar(t, torrentsDir, `{"version":1,"mode":"inject","rate_bytes_per_second":1048576,"multiplier":0,"jitter_fraction":0,"cap_bytes":0}`)

	var logs bytes.Buffer
	result, err := migrateLegacyExperimentSettings(configPath, torrentsDir, migrationLogger(&logs))
	if err != nil {
		t.Fatalf("migrateLegacyExperimentSettings: %v", err)
	}
	if !result.Migrated {
		t.Fatalf("migration did not merge; logs:\n%s", logs.String())
	}
	loaded, err := config.LoadWithEnvironment(configPath, emptyEnvironment{})
	if err != nil {
		t.Fatalf("reload migrated configuration: %v", err)
	}
	settings, err := experiments.SettingsFrom(loaded.Experiments.Upload)
	if err != nil {
		t.Fatalf("SettingsFrom: %v", err)
	}
	if settings.JitterFraction != 0 {
		t.Fatalf("jitter = %v, want the explicit zero of the sidecar", settings.JitterFraction)
	}
}

// TestMigrateLegacyExperimentSettingsFallsBackWhenTheFileIsNotWritable covers
// the read-only upgrade path: the file is untouched, the sidecar stays in
// place, and the running process keeps the legacy values with a warning.
func TestMigrateLegacyExperimentSettingsFallsBackWhenTheFileIsNotWritable(t *testing.T) {
	work := t.TempDir()
	torrentsDir := filepath.Join(work, "torrents")
	configPath := filepath.Join(work, "torrentfs.toml")
	document := "[log]\nlevel = \"debug\"\n"
	if err := os.WriteFile(configPath, []byte(document), 0o444); err != nil {
		t.Fatalf("write configuration: %v", err)
	}
	sidecar := writeLegacyExperimentSidecar(t, torrentsDir, `{"version":1,"mode":"amplify","rate_bytes_per_second":0,"multiplier":3,"jitter_fraction":0.1,"cap_bytes":0}`)

	var logs bytes.Buffer
	result, err := migrateLegacyExperimentSettings(configPath, torrentsDir, migrationLogger(&logs))
	if err != nil {
		t.Fatalf("migrateLegacyExperimentSettings: %v", err)
	}
	if result.Migrated {
		t.Fatal("migration reports a merge although the file is read-only")
	}
	if result.Runtime == nil {
		t.Fatal("migration reports no runtime fallback")
	}
	if result.Runtime.Mode != experiments.ModeAmplify || result.Runtime.Multiplier != 3 {
		t.Fatalf("runtime fallback = %+v, want amplify x3", *result.Runtime)
	}
	if !strings.Contains(logs.String(), "not migrated") && !strings.Contains(logs.String(), "尚未迁移") {
		t.Fatalf("migration did not warn about the missing merge; logs:\n%s", logs.String())
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("the sidecar was archived although the migration fell back: %v", err)
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read configuration: %v", err)
	}
	if string(content) != document {
		t.Fatalf("the read-only configuration changed:\n%s", content)
	}
}

// TestMigrateLegacyExperimentSettingsFallsBackWithoutAConfigurationFile covers
// a deployment that starts without a configuration file: the legacy values
// run, and the warning says they are not migrated.
func TestMigrateLegacyExperimentSettingsFallsBackWithoutAConfigurationFile(t *testing.T) {
	torrentsDir := filepath.Join(t.TempDir(), "torrents")
	sidecar := writeLegacyExperimentSidecar(t, torrentsDir, `{"version":1,"mode":"inject","rate_bytes_per_second":1048576,"multiplier":0,"jitter_fraction":0.2,"cap_bytes":0}`)

	var logs bytes.Buffer
	result, err := migrateLegacyExperimentSettings("", torrentsDir, migrationLogger(&logs))
	if err != nil {
		t.Fatalf("migrateLegacyExperimentSettings: %v", err)
	}
	if result.Runtime == nil || result.Runtime.Mode != experiments.ModeInject {
		t.Fatalf("runtime fallback = %+v, want inject", result.Runtime)
	}
	if result.Migrated {
		t.Fatal("migration reports a merge without a configuration file")
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("the sidecar is gone: %v", err)
	}
	if logs.Len() == 0 {
		t.Fatal("migration did not warn about the unmigrated settings")
	}
}

// TestMigrateLegacyExperimentSettingsWithoutASidecar covers the ordinary start:
// there is nothing to migrate and the configuration is untouched.
func TestMigrateLegacyExperimentSettingsWithoutASidecar(t *testing.T) {
	work := t.TempDir()
	configPath := filepath.Join(work, "torrentfs.toml")
	document := "[log]\nlevel = \"debug\"\n"
	if err := os.WriteFile(configPath, []byte(document), 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}

	var logs bytes.Buffer
	result, err := migrateLegacyExperimentSettings(configPath, filepath.Join(work, "torrents"), migrationLogger(&logs))
	if err != nil {
		t.Fatalf("migrateLegacyExperimentSettings: %v", err)
	}
	if result.Migrated || result.Runtime != nil {
		t.Fatalf("migration did something without a sidecar: %+v", result)
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read configuration: %v", err)
	}
	if string(content) != document {
		t.Fatalf("the configuration changed without a sidecar:\n%s", content)
	}
	if logs.Len() != 0 {
		t.Fatalf("the ordinary start logged a migration warning:\n%s", logs.String())
	}
}

// TestMigrateLegacyExperimentSettingsRejectsACorruptSidecar covers the
// contract that an unreadable sidecar is reported instead of being ignored:
// silently dropping the operator's experiment settings is worse than a start
// that names the file.
func TestMigrateLegacyExperimentSettingsRejectsACorruptSidecar(t *testing.T) {
	work := t.TempDir()
	configPath := filepath.Join(work, "torrentfs.toml")
	if err := os.WriteFile(configPath, []byte("[log]\nlevel = \"debug\"\n"), 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}
	writeLegacyExperimentSidecar(t, filepath.Join(work, "torrents"), `{"version":1,"mode":`)

	var logs bytes.Buffer
	if _, err := migrateLegacyExperimentSettings(configPath, filepath.Join(work, "torrents"), migrationLogger(&logs)); err == nil {
		t.Fatal("migrateLegacyExperimentSettings accepted a corrupt sidecar")
	}
}
