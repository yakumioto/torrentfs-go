package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
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

func TestExpandTorrentInputScansDirectory(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"z.torrent", "a.torrent", "ignore.txt", "upper.TORRENT"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not parsed here"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatalf("make nested directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "nested.torrent"), []byte("nested"), 0o644); err != nil {
		t.Fatalf("write nested torrent: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "directory.torrent"), 0o755); err != nil {
		t.Fatalf("make torrent-named directory: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "a.torrent"), filepath.Join(dir, "link.torrent")); err != nil {
		t.Fatalf("make torrent symlink: %v", err)
	}

	got, err := expandTorrentInput(dir)
	if err != nil {
		t.Fatalf("expandTorrentInput: %v", err)
	}
	want := []string{
		filepath.Join(dir, "a.torrent"),
		filepath.Join(dir, "z.torrent"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expanded paths = %#v, want %#v", got, want)
	}
}

func TestRunDirectoryInputReportsExpandedTorrentError(t *testing.T) {
	work := t.TempDir()
	torrentDir := filepath.Join(work, "torrents")
	if err := os.Mkdir(torrentDir, 0o755); err != nil {
		t.Fatalf("make torrent directory: %v", err)
	}
	badTorrent := filepath.Join(torrentDir, "bad.torrent")
	if err := os.WriteFile(badTorrent, []byte("not a torrent"), 0o644); err != nil {
		t.Fatalf("write bad torrent: %v", err)
	}

	var stderr bytes.Buffer
	code := run([]string{
		"-mountpoint", filepath.Join(work, "mnt"),
		"-data-dir", filepath.Join(work, "data"),
		torrentDir,
	}, &stderr)
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), badTorrent) {
		t.Fatalf("stderr = %q, want expanded torrent path", stderr.String())
	}
}

func TestExpandTorrentInputKeepsNonDirectories(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file.data")
	if err := os.WriteFile(file, []byte("file"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	link := filepath.Join(dir, "link.data")
	if err := os.Symlink(file, link); err != nil {
		t.Fatalf("make file symlink: %v", err)
	}
	missing := filepath.Join(dir, "missing.torrent")

	for _, input := range []string{file, link, missing} {
		got, err := expandTorrentInput(input)
		if err != nil {
			t.Fatalf("expandTorrentInput(%q): %v", input, err)
		}
		want := []string{input}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("expandTorrentInput(%q) = %#v, want %#v", input, got, want)
		}
	}
}

func TestExpandTorrentInputAcceptsEmptyDirectory(t *testing.T) {
	got, err := expandTorrentInput(t.TempDir())
	if err != nil {
		t.Fatalf("expandTorrentInput: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expanded paths = %#v, want empty", got)
	}
}
