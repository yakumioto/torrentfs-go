package configmigrate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrateUploadRateWithCredentialsFromTheEnvironment covers the SMB
// deployment whose credentials are environment variables: the file layer has
// no credentials, and the migration must still fold the retired sidecar into
// the configuration file.
func TestMigrateUploadRateWithCredentialsFromTheEnvironment(t *testing.T) {
	t.Setenv("TORRENTFS_USERNAME", "torrentfs")
	t.Setenv("TORRENTFS_PASSWORD", "s3cr3t")

	dir := t.TempDir()
	configPath := filepath.Join(dir, "torrentfs.toml")
	writeTestFile(t, configPath, "backend = \"smb\"\n")
	torrentsDir := filepath.Join(dir, "torrents")
	writeTestFile(t, sidecarPath(torrentsDir), legacySidecarBody)

	cfg, sources := loadTestConfig(t, configPath)
	result, _ := runMigration(t, configPath, torrentsDir, cfg, sources)

	if !result.Migrated {
		t.Fatal("the migration did not report the sidecar as migrated")
	}
	if result.LegacyInUse {
		t.Error("the migration kept the retired sidecar as the runtime source")
	}
	if _, err := os.Stat(sidecarPath(torrentsDir) + ".migrated"); err != nil {
		t.Errorf("the sidecar was not archived: %v", err)
	}
	document, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read migrated configuration: %v", err)
	}
	if !strings.Contains(string(document), "upload_rate") {
		t.Errorf("migrated configuration = %s", document)
	}
}
