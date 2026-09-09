package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunMissingConfigReturnsConfigurationExitCode(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.toml")
	var stderr bytes.Buffer

	code := run([]string{
		"-mountpoint", t.TempDir(),
		"-config", missing,
	}, &stderr)
	if code != 2 {
		t.Fatalf("run exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), missing) {
		t.Fatalf("stderr = %q, want missing config path", stderr.String())
	}
}

func TestRunInvalidConfigReturnsConfigurationExitCode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.toml")
	if err := os.WriteFile(path, []byte("[paths]\ndata_dir = \"\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var stderr bytes.Buffer

	code := run([]string{
		"-mountpoint", t.TempDir(),
		"-config", path,
	}, &stderr)
	if code != 2 {
		t.Fatalf("run exit code = %d, want 2", code)
	}
	for _, want := range []string{path, "paths.data_dir", "value is required"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	}
}

func TestLoadConfigAppliesExplicitDataDirOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[paths]\ndata_dir = \"from-file\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	override := filepath.Join(t.TempDir(), "override")

	cfg, err := loadConfig(path, override, true)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Paths.DataDir != override {
		t.Fatalf("DataDir = %q, want %q", cfg.Paths.DataDir, override)
	}
}

func TestLoadConfigKeepsFileDataDirWhenFlagWasOmitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	dataDir := filepath.Join(t.TempDir(), "from-file")
	if err := os.WriteFile(path, []byte("[paths]\ndata_dir = \""+dataDir+"\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := loadConfig(path, "", false)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Paths.DataDir != dataDir {
		t.Fatalf("DataDir = %q, want %q", cfg.Paths.DataDir, dataDir)
	}
}
