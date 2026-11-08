package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeContentFile writes a configuration file for the content-loading tests.
func writeContentFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "torrentfs.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write configuration file: %v", err)
	}
	return path
}

// TestLoadContentWithSourcesMatchesThePathLoader covers the contract that a
// document held in memory is loaded exactly like the same bytes on disk: the
// save path validates its rendered document through this loader, so the two
// must not drift apart.
func TestLoadContentWithSourcesMatchesThePathLoader(t *testing.T) {
	body := "backend = \"none\"\n[log]\nlevel = \"info\"\n"
	path := writeContentFile(t, body)
	t.Setenv("TORRENTFS_LOG_LEVEL", "debug")

	fromPath, pathSources, err := LoadWithSources(path)
	if err != nil {
		t.Fatalf("LoadWithSources: %v", err)
	}
	fromContent, contentSources, err := LoadContentWithSources([]byte(body), path)
	if err != nil {
		t.Fatalf("LoadContentWithSources: %v", err)
	}
	if !reflect.DeepEqual(fromContent, fromPath) {
		t.Errorf("content configuration = %+v, want the path configuration %+v", fromContent, fromPath)
	}
	if !reflect.DeepEqual(contentSources, pathSources) {
		t.Errorf("content sources = %v, want the path sources %v", contentSources, pathSources)
	}
}

// TestLoadContentWithEnvironmentReadsTheFileLayer covers the write-back input:
// the file layer is the built-in defaults overlaid with the document, before
// the environment is merged.
func TestLoadContentWithEnvironmentReadsTheFileLayer(t *testing.T) {
	body := "[log]\nlevel = \"warn\"\n"
	t.Setenv("TORRENTFS_LOG_LEVEL", "debug")

	cfg, err := LoadContentWithEnvironment([]byte(body), "torrentfs.toml", emptyEnvironment{})
	if err != nil {
		t.Fatalf("LoadContentWithEnvironment: %v", err)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("log.level = %q, want the file value warn", cfg.Log.Level)
	}
}

// TestLoadContentNamesTheDocumentInErrors covers the contract that the save
// path reports the configuration file the user edits, not a temporary copy.
func TestLoadContentNamesTheDocumentInErrors(t *testing.T) {
	_, _, err := LoadContentWithSources([]byte("[cache]\ncapacity = \"0B\"\n"), "/etc/torrentfs/torrentfs.toml")
	if err == nil {
		t.Fatal("LoadContentWithSources accepted an invalid capacity")
	}
	if !strings.Contains(err.Error(), "/etc/torrentfs/torrentfs.toml") {
		t.Errorf("the error does not name the configuration file: %v", err)
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("the error does not report ErrInvalid: %v", err)
	}
}

// TestLoadContentRejectsTheRemovedKeys covers the strictness of the content
// loader: a rendered document is still a configuration document, so a removed
// key is reported exactly as a start would report it.
func TestLoadContentRejectsTheRemovedKeys(t *testing.T) {
	_, _, err := LoadContentWithSources([]byte("[lab]\nupload_mode = \"baseline\"\n"), "torrentfs.toml")
	if err == nil {
		t.Fatal("LoadContentWithSources accepted a removed configuration section")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("the error does not report ErrInvalid: %v", err)
	}
}
