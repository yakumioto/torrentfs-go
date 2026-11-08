package configmigrate_test

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/configmigrate"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

const legacySidecarBody = `{"version":1,"rate_limit_bytes_per_second":1048576,"schedule":{"start":"08:00","end":"22:00"}}`

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("make directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func loadTestConfig(t *testing.T, path string) (config.Config, config.Sources) {
	t.Helper()
	cfg, sources, err := config.LoadWithSources(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return cfg, sources
}

func sidecarPath(torrentsDir string) string {
	return filepath.Join(torrentsDir, ".metadata", "upload_rate.json")
}

func runMigration(t *testing.T, configPath, torrentsDir string, cfg config.Config, sources config.Sources) (configmigrate.Result, string) {
	t.Helper()
	var log bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	result, err := configmigrate.MigrateUploadRate(configmigrate.Options{
		ConfigPath:  configPath,
		TorrentsDir: torrentsDir,
		Config:      cfg,
		Sources:     sources,
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("MigrateUploadRate: %v", err)
	}
	return result, log.String()
}

func assertRuntimeSettings(t *testing.T, cfg config.Config, want session.UploadRateSettings) {
	t.Helper()
	got, err := session.UploadRateSettingsFromConfig(cfg)
	if err != nil {
		t.Fatalf("UploadRateSettingsFromConfig: %v", err)
	}
	if got.RateLimitBytesPerSecond != want.RateLimitBytesPerSecond {
		t.Errorf("runtime rate = %d, want %d", got.RateLimitBytesPerSecond, want.RateLimitBytesPerSecond)
	}
	switch {
	case got.Schedule == nil && want.Schedule == nil:
	case got.Schedule == nil || want.Schedule == nil:
		t.Errorf("runtime schedule = %+v, want %+v", got.Schedule, want.Schedule)
	default:
		if *got.Schedule != *want.Schedule {
			t.Errorf("runtime schedule = %+v, want %+v", got.Schedule, want.Schedule)
		}
	}
}

// TestMigrateUploadRateIntoAWritableConfiguration covers the upgrade path of a
// writable deployment: the sidecar values join the file layer (only the keys
// the file does not already carry), the sidecar is archived, and the runtime
// keeps the limit it had before the upgrade.
func TestMigrateUploadRateIntoAWritableConfiguration(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "torrentfs.toml")
	writeTestFile(t, configPath, "[log]\nlevel = \"info\"\n")
	torrentsDir := filepath.Join(dir, "torrents")
	legacyPath := sidecarPath(torrentsDir)
	writeTestFile(t, legacyPath, legacySidecarBody)

	cfg, sources := loadTestConfig(t, configPath)
	result, logOutput := runMigration(t, configPath, torrentsDir, cfg, sources)

	if !result.Migrated {
		t.Error("the migration did not report the sidecar as migrated")
	}
	if result.LegacyInUse {
		t.Error("the migration reported the retired sidecar as the runtime source")
	}
	assertRuntimeSettings(t, result.Config, session.UploadRateSettings{
		RateLimitBytesPerSecond: 1 << 20,
		Schedule:                &session.UploadRateSchedule{Start: "08:00", End: "22:00"},
	})
	for _, path := range []string{"upload_rate.limit", "upload_rate.schedule_start", "upload_rate.schedule_end"} {
		if !result.Sources.FromFile(path) {
			t.Errorf("the source of %q = %q, want file", path, result.Sources.SourceOf(path))
		}
	}

	document, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read the migrated configuration: %v", err)
	}
	rendered := string(document)
	for _, want := range []string{
		"[upload_rate]",
		`limit = "1MiB/s"`,
		`schedule_start = "08:00"`,
		`schedule_end = "22:00"`,
		"[log]",
		`level = "info"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the migrated configuration does not hold %q:\n%s", want, rendered)
		}
	}

	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the sidecar still exists after migration: %v", err)
	}
	archived, err := os.ReadFile(legacyPath + ".migrated")
	if err != nil {
		t.Fatalf("read the archived sidecar: %v", err)
	}
	if string(archived) != legacySidecarBody {
		t.Errorf("the archived sidecar = %q, want the original bytes", archived)
	}
	if !strings.Contains(logOutput, "migrated") {
		t.Errorf("the migration log does not report the migration: %s", logOutput)
	}
}

// TestMigrateUploadRateOnlyFillsMissingKeys covers the sparse contract: a key
// the configuration file already carries wins over the sidecar, and the
// remaining keys still join the file.
func TestMigrateUploadRateOnlyFillsMissingKeys(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "torrentfs.toml")
	writeTestFile(t, configPath, "[upload_rate]\nlimit = \"2MiB/s\"\n")
	torrentsDir := filepath.Join(dir, "torrents")
	writeTestFile(t, sidecarPath(torrentsDir), legacySidecarBody)

	cfg, sources := loadTestConfig(t, configPath)
	result, _ := runMigration(t, configPath, torrentsDir, cfg, sources)

	document, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read the migrated configuration: %v", err)
	}
	rendered := string(document)
	if !strings.Contains(rendered, `limit = "2MiB/s"`) {
		t.Errorf("the migration replaced the file limit:\n%s", rendered)
	}
	if strings.Contains(rendered, `limit = "1MiB/s"`) {
		t.Errorf("the migration wrote the sidecar limit over the file value:\n%s", rendered)
	}
	for _, want := range []string{`schedule_start = "08:00"`, `schedule_end = "22:00"`} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the migration did not add the missing %q:\n%s", want, rendered)
		}
	}
	assertRuntimeSettings(t, result.Config, session.UploadRateSettings{
		RateLimitBytesPerSecond: 2 << 20,
		Schedule:                &session.UploadRateSchedule{Start: "08:00", End: "22:00"},
	})
}

// TestMigrateUploadRateWithoutASidecarIsANoOp covers a deployment that never
// used the retired settings.
func TestMigrateUploadRateWithoutASidecarIsANoOp(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "torrentfs.toml")
	writeTestFile(t, configPath, "[log]\nlevel = \"debug\"\n")
	torrentsDir := filepath.Join(dir, "torrents")

	cfg, sources := loadTestConfig(t, configPath)
	result, logOutput := runMigration(t, configPath, torrentsDir, cfg, sources)

	if result.Migrated || result.LegacyInUse {
		t.Errorf("the migration reported work without a sidecar: %+v", result)
	}
	if logOutput != "" {
		t.Errorf("the migration logged about a missing sidecar: %s", logOutput)
	}
}

// TestMigrateUploadRateWithoutAConfigurationFileKeepsTheLegacyRuntime covers a
// deployment that started without a configuration file: the sidecar value
// stays the runtime start value, the page cannot save it, and the startup
// warns that the migration is pending.
func TestMigrateUploadRateWithoutAConfigurationFileKeepsTheLegacyRuntime(t *testing.T) {
	torrentsDir := filepath.Join(t.TempDir(), "torrents")
	writeTestFile(t, sidecarPath(torrentsDir), legacySidecarBody)

	cfg := config.Default()
	result, logOutput := runMigration(t, "", torrentsDir, cfg, config.Sources{})

	if result.Migrated {
		t.Error("the migration reported a file migration without a configuration file")
	}
	if !result.LegacyInUse {
		t.Error("the migration did not report the legacy runtime value")
	}
	assertRuntimeSettings(t, result.Config, session.UploadRateSettings{
		RateLimitBytesPerSecond: 1 << 20,
		Schedule:                &session.UploadRateSchedule{Start: "08:00", End: "22:00"},
	})
	if !strings.Contains(logOutput, "legacy") && !strings.Contains(logOutput, "not migrated") {
		t.Errorf("the startup log does not warn about the pending migration: %s", logOutput)
	}
	if _, err := os.Stat(sidecarPath(torrentsDir)); err != nil {
		t.Errorf("the sidecar disappeared without a configuration file: %v", err)
	}
}

// TestMigrateUploadRateFromAReadOnlyConfigurationKeepsTheLegacyRuntime covers
// the read-only deployment: the file cannot be written, so the sidecar value
// keeps running and the startup warns.
func TestMigrateUploadRateFromAReadOnlyConfigurationKeepsTheLegacyRuntime(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a root process can write a read-only file, so the read-only contract cannot be observed")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "torrentfs.toml")
	writeTestFile(t, configPath, "[log]\nlevel = \"info\"\n")
	if err := os.Chmod(configPath, 0o444); err != nil {
		t.Fatalf("make the configuration read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(configPath, 0o644) })
	torrentsDir := filepath.Join(dir, "torrents")
	writeTestFile(t, sidecarPath(torrentsDir), legacySidecarBody)

	cfg, sources := loadTestConfig(t, configPath)
	result, logOutput := runMigration(t, configPath, torrentsDir, cfg, sources)

	if result.Migrated {
		t.Error("the migration claimed to write a read-only configuration")
	}
	if !result.LegacyInUse {
		t.Error("the migration did not report the legacy runtime value")
	}
	assertRuntimeSettings(t, result.Config, session.UploadRateSettings{
		RateLimitBytesPerSecond: 1 << 20,
		Schedule:                &session.UploadRateSchedule{Start: "08:00", End: "22:00"},
	})
	document, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read the read-only configuration: %v", err)
	}
	if strings.Contains(string(document), "upload_rate") {
		t.Errorf("the migration changed the read-only configuration:\n%s", document)
	}
	if !strings.Contains(logOutput, "not migrated") {
		t.Errorf("the startup log does not warn about the pending migration: %s", logOutput)
	}
	if _, err := os.Stat(sidecarPath(torrentsDir)); err != nil {
		t.Errorf("the sidecar disappeared from a read-only deployment: %v", err)
	}
}

// TestMigrateUploadRateKeepsEnvironmentValuesOnTheLegacyRuntime covers the
// documented precedence during a read-only downgrade: the environment is a
// configuration source, so the retired sidecar must not replace a value the
// environment supplies. The page shows the same values the session uses.
func TestMigrateUploadRateKeepsEnvironmentValuesOnTheLegacyRuntime(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a root process can write a read-only file, so the read-only contract cannot be observed")
	}
	t.Setenv("TORRENTFS_UPLOAD_RATE_LIMIT", "2MiB/s")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "torrentfs.toml")
	writeTestFile(t, configPath, "[log]\nlevel = \"info\"\n")
	if err := os.Chmod(configPath, 0o444); err != nil {
		t.Fatalf("make the configuration read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(configPath, 0o644) })
	torrentsDir := filepath.Join(dir, "torrents")
	writeTestFile(t, sidecarPath(torrentsDir), legacySidecarBody)

	cfg, sources := loadTestConfig(t, configPath)
	result, logOutput := runMigration(t, configPath, torrentsDir, cfg, sources)

	if result.Migrated || !result.LegacyInUse {
		t.Fatalf("the read-only migration result = %+v, want the legacy runtime", result)
	}
	// The environment limit wins; the sidecar still supplies the window the
	// environment does not set.
	assertRuntimeSettings(t, result.Config, session.UploadRateSettings{
		RateLimitBytesPerSecond: 2 << 20,
		Schedule:                &session.UploadRateSchedule{Start: "08:00", End: "22:00"},
	})
	if got := result.Sources.SourceOf("upload_rate.limit"); got != config.SourceEnvironment {
		t.Errorf("the source of upload_rate.limit = %q, want environment", got)
	}
	for _, path := range []string{"upload_rate.schedule_start", "upload_rate.schedule_end"} {
		if got := result.Sources.SourceOf(path); got != config.SourceFile {
			t.Errorf("the source of %q = %q, want the closest source file", path, got)
		}
	}
	if !strings.Contains(logOutput, "not migrated") {
		t.Errorf("the startup log does not warn about the pending migration: %s", logOutput)
	}
}

// TestMigrateUploadRateArchivesOnTheNextStartAfterASuccessfulSave covers the
// read-only deployment that later became writable: the next start imports the
// still-missing sidecar keys into the file (the save may not have touched
// them), archives the sidecar, and keeps the limit.
func TestMigrateUploadRateArchivesOnTheNextStartAfterASuccessfulSave(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a root process can write a read-only file, so the read-only contract cannot be observed")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "torrentfs.toml")
	writeTestFile(t, configPath, "[log]\nlevel = \"info\"\n")
	torrentsDir := filepath.Join(dir, "torrents")
	legacyPath := sidecarPath(torrentsDir)
	writeTestFile(t, legacyPath, legacySidecarBody)

	if err := os.Chmod(configPath, 0o444); err != nil {
		t.Fatalf("make the configuration read-only: %v", err)
	}
	cfg, sources := loadTestConfig(t, configPath)
	first, _ := runMigration(t, configPath, torrentsDir, cfg, sources)
	if !first.LegacyInUse || first.Migrated {
		t.Fatalf("the first start = %+v, want the legacy runtime and no migration", first)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("the sidecar disappeared before a successful save: %v", err)
	}

	// The operator made the file writable and the configuration page saved an
	// unrelated key.
	if err := os.Chmod(configPath, 0o644); err != nil {
		t.Fatalf("make the configuration writable: %v", err)
	}
	writeTestFile(t, configPath, "[log]\nlevel = \"debug\"\n")
	cfg, sources = loadTestConfig(t, configPath)
	second, _ := runMigration(t, configPath, torrentsDir, cfg, sources)

	if !second.Migrated {
		t.Errorf("the start after the save did not migrate the sidecar: %+v", second)
	}
	if second.LegacyInUse {
		t.Error("the start after the save kept the legacy runtime instead of the file")
	}
	document, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read the configuration: %v", err)
	}
	for _, want := range []string{`level = "debug"`, `limit = "1MiB/s"`, `schedule_start = "08:00"`, `schedule_end = "22:00"`} {
		if !strings.Contains(string(document), want) {
			t.Errorf("the configuration after the next start does not hold %q:\n%s", want, document)
		}
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the sidecar still exists after the next start: %v", err)
	}
	if _, err := os.Stat(legacyPath + ".migrated"); err != nil {
		t.Errorf("the sidecar was not archived: %v", err)
	}
}

// TestMigrateUploadRateRejectsAnUnreadableSidecar covers the fail-start
// contract inherited from the retired sidecar loader: a file the daemon cannot
// trust must not silently become unlimited.
func TestMigrateUploadRateRejectsAnUnreadableSidecar(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "torrentfs.toml")
	writeTestFile(t, configPath, "[log]\nlevel = \"info\"\n")
	torrentsDir := filepath.Join(dir, "torrents")
	writeTestFile(t, sidecarPath(torrentsDir), `{"version":1,"rate_limit_bytes_per_second":1024,"extra":true}`)

	cfg, sources := loadTestConfig(t, configPath)
	_, err := configmigrate.MigrateUploadRate(configmigrate.Options{
		ConfigPath:  configPath,
		TorrentsDir: torrentsDir,
		Config:      cfg,
		Sources:     sources,
	})
	if err == nil {
		t.Fatal("MigrateUploadRate accepted an unreadable sidecar")
	}
	if !strings.Contains(err.Error(), "upload_rate.json") {
		t.Errorf("error %q does not name the sidecar", err)
	}
}

// TestMigrateUploadRateFallsBackWhenTheWriteBackFails covers a write failure
// the permission check cannot see: the sidecar stays the runtime value and the
// file is unchanged.
func TestMigrateUploadRateFallsBackWhenTheWriteBackFails(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "torrentfs.toml")
	writeTestFile(t, configPath, "[log]\nlevel = \"info\"\n")
	torrentsDir := filepath.Join(dir, "torrents")
	writeTestFile(t, sidecarPath(torrentsDir), legacySidecarBody)
	cfg, sources := loadTestConfig(t, configPath)

	var log bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&log, nil))
	result, err := configmigrate.MigrateUploadRate(configmigrate.Options{
		ConfigPath:  configPath,
		TorrentsDir: torrentsDir,
		Config:      cfg,
		Sources:     sources,
		Logger:      logger,
		WriteBackFile: func(string, []byte) (bool, error) {
			return false, config.ErrWriteBackUnavailable
		},
	})
	if err != nil {
		t.Fatalf("MigrateUploadRate: %v", err)
	}
	if result.Migrated || !result.LegacyInUse {
		t.Fatalf("the failed write-back result = %+v, want the legacy runtime", result)
	}
	assertRuntimeSettings(t, result.Config, session.UploadRateSettings{
		RateLimitBytesPerSecond: 1 << 20,
		Schedule:                &session.UploadRateSchedule{Start: "08:00", End: "22:00"},
	})
	if !strings.Contains(log.String(), "not migrated") {
		t.Errorf("the failed migration does not warn: %s", log.String())
	}
	if _, err := os.Stat(sidecarPath(torrentsDir)); err != nil {
		t.Errorf("the sidecar disappeared after a failed write-back: %v", err)
	}
}
