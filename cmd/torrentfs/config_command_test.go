package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfigFile writes a configuration file and returns its path.
func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "torrentfs.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestConfigCheckAcceptsAUsableConfiguration covers the contract that the
// check says nothing when the configuration is usable.
func TestConfigCheckAcceptsAUsableConfiguration(t *testing.T) {
	var stderr strings.Builder
	code := run([]string{"config", "check", "--config", writeConfigFile(t, "backend = \"none\"\n")}, &stderr)
	if code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("run printed %q, want nothing", stderr.String())
	}
}

// TestConfigCheckReportsEveryProblem covers the contract that a user who
// changed several values learns about all of them at once.
func TestConfigCheckReportsEveryProblem(t *testing.T) {
	body := "backend = \"fuse\"\n[cache]\ncapacity = \"0B\"\n[log]\nlevel = \"nonsense\"\n"
	var stderr strings.Builder
	code := run([]string{"config", "check", "--config", writeConfigFile(t, body)}, &stderr)
	if code != 2 {
		t.Fatalf("run = %d, want 2; stderr: %s", code, stderr.String())
	}
	for _, field := range []string{"cache.capacity", "log.level", "fuse.mountpoint"} {
		if !strings.Contains(stderr.String(), field) {
			t.Errorf("the report does not name %q: %s", field, stderr.String())
		}
	}
}

// TestConfigCheckCoversTheExperiment covers the contract that the check
// covers the upload statistics experiment: its rules relate one setting to
// another, so a configuration that names the experiment wrongly is not
// usable.
func TestConfigCheckCoversTheExperiment(t *testing.T) {
	body := "[lab]\nupload_mode = \"baseline\"\nupload_rate = \"1MiB/s\"\n"
	var stderr strings.Builder
	code := run([]string{"config", "check", "--config", writeConfigFile(t, body)}, &stderr)
	if code != 2 {
		t.Fatalf("run = %d, want 2; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "upload_rate") {
		t.Errorf("the report does not name the experiment setting: %s", stderr.String())
	}
}

// TestConfigCheckNeedsNoTorrentsDirectory covers the contract that the
// configuration commands work on the configuration alone.
func TestConfigCheckNeedsNoTorrentsDirectory(t *testing.T) {
	var stderr strings.Builder
	code := run([]string{"config", "check"}, &stderr)
	if code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr.String())
	}
}

// TestConfigPrintRedactsTheSecret covers the contract that the printed
// configuration never discloses a password.
func TestConfigPrintRedactsTheSecret(t *testing.T) {
	const secret = "a-password-that-must-not-leak"
	body := "username = \"reader\"\npassword = \"" + secret + "\"\n"
	path := writeConfigFile(t, body)

	for _, format := range []string{"toml", "json"} {
		var stderr strings.Builder
		code := run([]string{"config", "print", "--config", path, "--format", format}, &stderr)
		if code != 0 {
			t.Fatalf("run = %d, want 0; stderr: %s", code, stderr.String())
		}
		if strings.Contains(stderr.String(), secret) {
			t.Errorf("the %s output disclosed the password: %s", format, stderr.String())
		}
		if !strings.Contains(stderr.String(), "<redacted>") {
			t.Errorf("the %s output does not mark the redacted value: %s", format, stderr.String())
		}
	}
}

// TestConfigPrintNamesTheSource covers the contract that the printed
// configuration names the input every value came from.
func TestConfigPrintNamesTheSource(t *testing.T) {
	path := writeConfigFile(t, "backend = \"none\"\n[connections]\nlisten_port = 12345\n")

	var stderr strings.Builder
	if code := run([]string{"config", "print", "--config", path, "--sources"}, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "source: file") {
		t.Errorf("the output does not name the file as the source: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "source: default") {
		t.Errorf("the output does not name the default as the source: %s", stderr.String())
	}
}

// TestConfigPrintWithoutSources covers the contract that the source of a
// value is printed only when it is asked for.
func TestConfigPrintWithoutSources(t *testing.T) {
	var stderr strings.Builder
	if code := run([]string{"config", "print"}, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "source:") {
		t.Errorf("the output names the sources without being asked: %s", stderr.String())
	}
}

// TestConfigPrintJSON covers the contract that the printed configuration has
// a JSON form whose values are the effective values.
func TestConfigPrintJSON(t *testing.T) {
	path := writeConfigFile(t, "backend = \"none\"\n[connections]\nlisten_port = 12345\n")

	var stderr strings.Builder
	if code := run([]string{"config", "print", "--config", path, "--format", "json"}, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr.String())
	}
	var document struct {
		Values map[string]string `json:"values"`
	}
	if err := json.Unmarshal([]byte(stderr.String()), &document); err != nil {
		t.Fatalf("the output is not JSON: %v; output: %s", err, stderr.String())
	}
	if document.Values["backend"] != "none" {
		t.Errorf("backend = %q, want none", document.Values["backend"])
	}
	if document.Values["connections.listen_port"] != "12345" {
		t.Errorf("connections.listen_port = %q, want 12345", document.Values["connections.listen_port"])
	}
}

// TestConfigPrintRejectsAnUnknownFormat covers the contract that an unknown
// output format is reported instead of guessed.
func TestConfigPrintRejectsAnUnknownFormat(t *testing.T) {
	var stderr strings.Builder
	code := run([]string{"config", "print", "--format", "yaml"}, &stderr)
	if code != 2 {
		t.Fatalf("run = %d, want 2; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "yaml") {
		t.Errorf("the report does not name the format: %s", stderr.String())
	}
}

// TestConfigCommandNeedsNoTorrentsDirectory covers the contract that the
// configuration commands do not take a torrents directory.
func TestConfigCommandNeedsNoTorrentsDirectory(t *testing.T) {
	var stderr strings.Builder
	if code := run([]string{"config", "check", "--config", writeConfigFile(t, "backend = \"none\"\n")}, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr.String())
	}
}

// TestConfigCommandRejectsAnUnknownCommand covers the contract that an
// unknown configuration command is reported.
func TestConfigCommandRejectsAnUnknownCommand(t *testing.T) {
	var stderr strings.Builder
	code := run([]string{"config", "explain"}, &stderr)
	if code != 2 {
		t.Fatalf("run = %d, want 2; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "explain") {
		t.Errorf("the report does not name the command: %s", stderr.String())
	}
}

// TestStartupStillNeedsATorrentsDirectory covers the contract that the
// configuration commands did not change how a start works: a start still
// needs exactly one torrents directory.
func TestStartupStillNeedsATorrentsDirectory(t *testing.T) {
	var stderr strings.Builder
	code := run([]string{}, &stderr)
	if code != 2 {
		t.Fatalf("run = %d, want 2; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "torrents directory") {
		t.Errorf("the report does not ask for a torrents directory: %s", stderr.String())
	}
}
