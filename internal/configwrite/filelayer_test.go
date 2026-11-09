package configwrite_test

import (
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/configwrite"
)

// TestLoadReadsAFileWhoseRequirementsComeFromTheEnvironment covers the SMB
// deployment whose credentials are environment variables: the file layer has
// no credentials, so it must load, and the rendered write-back must validate
// against the environment instead of the bare file.
func TestLoadReadsAFileWhoseRequirementsComeFromTheEnvironment(t *testing.T) {
	t.Setenv("TORRENTFS_USERNAME", "torrentfs")
	t.Setenv("TORRENTFS_PASSWORD", "s3cr3t")

	file, err := configwrite.Load([]byte("backend = \"smb\"\n"), "torrentfs.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if file.Config.Backend != config.BackendSMB {
		t.Errorf("backend = %q, want smb", file.Config.Backend)
	}

	prepared, err := configwrite.Prepare(file, "torrentfs.toml", map[string]string{"log.level": "debug"}, false)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prepared.Effective.Log.Level != "debug" {
		t.Errorf("effective log.level = %q, want debug", prepared.Effective.Log.Level)
	}
}
