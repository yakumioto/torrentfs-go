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

// runCLICapture runs the command line and captures standard output and
// standard error separately, so a test can assert which stream carried what.
func runCLICapture(args []string) (code int, stdout, stderr string) {
	var out, errOut strings.Builder
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// TestConfigCheckAcceptsAUsableConfiguration covers the contract that the
// check says nothing when the configuration is usable.
func TestConfigCheckAcceptsAUsableConfiguration(t *testing.T) {
	code, stdout, stderr := runCLICapture([]string{"config", "check", "--config", writeConfigFile(t, "backend = \"none\"\n")})
	if code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr)
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("run printed stdout %q and stderr %q, want nothing", stdout, stderr)
	}
}

// TestConfigCheckReportsEveryProblem covers the contract that a user who
// changed several values learns about all of them at once.
func TestConfigCheckReportsEveryProblem(t *testing.T) {
	body := "backend = \"fuse\"\n[cache]\ncapacity = \"0B\"\n[log]\nlevel = \"nonsense\"\n"
	var stderr strings.Builder
	code := runCLI([]string{"config", "check", "--config", writeConfigFile(t, body)}, &stderr)
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
	code := runCLI([]string{"config", "check", "--config", writeConfigFile(t, body)}, &stderr)
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
	code := runCLI([]string{"config", "check"}, &stderr)
	if code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr.String())
	}
}

// printConfig runs `config print` and returns its standard output.
//
// The report is the result of the command, so it belongs on standard output:
// the helper fails the test when the command exits with a non-zero code, when
// it writes diagnostics, or when it writes nothing at all.
func printConfig(t *testing.T, args ...string) string {
	t.Helper()
	code, stdout, stderr := runCLICapture(append([]string{"config", "print"}, args...))
	if code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("the report went to standard error: %s", stderr)
	}
	if stdout == "" {
		t.Fatal("the report did not go to standard output")
	}
	return stdout
}

// TestConfigPrintRedactsTheSecret covers the contract that the printed
// configuration never discloses a password.
func TestConfigPrintRedactsTheSecret(t *testing.T) {
	const secret = "a-password-that-must-not-leak"
	path := writeConfigFile(t, "username = \"reader\"\npassword = \""+secret+"\"\n")

	for _, format := range []string{"toml", "json"} {
		output := printConfig(t, "--config", path, "--format", format)
		if strings.Contains(output, secret) {
			t.Errorf("the %s output disclosed the password: %s", format, output)
		}
		if !strings.Contains(output, "<redacted>") {
			t.Errorf("the %s output does not mark the redacted value: %s", format, output)
		}
	}
}

// TestConfigPrintNamesTheSource covers the contract that the printed
// configuration names the input every value came from.
func TestConfigPrintNamesTheSource(t *testing.T) {
	path := writeConfigFile(t, "backend = \"none\"\n[connections]\nlisten_port = 12345\n")

	output := printConfig(t, "--config", path, "--sources")
	if !strings.Contains(output, "source: file") {
		t.Errorf("the output does not name the file as the source: %s", output)
	}
	if !strings.Contains(output, "source: default") {
		t.Errorf("the output does not name the default as the source: %s", output)
	}
}

// TestConfigPrintWithoutSources covers the contract that the source of a
// value is printed only when it is asked for.
func TestConfigPrintWithoutSources(t *testing.T) {
	output := printConfig(t)
	if strings.Contains(output, "source:") {
		t.Errorf("the output names the sources without being asked: %s", output)
	}
}

// TestConfigPrintJSON covers the contract that the printed configuration has
// a JSON form whose values are the effective values.
func TestConfigPrintJSON(t *testing.T) {
	path := writeConfigFile(t, "backend = \"none\"\n[connections]\nlisten_port = 12345\n")

	output := printConfig(t, "--config", path, "--format", "json")
	var document struct {
		Values map[string]string `json:"values"`
	}
	if err := json.Unmarshal([]byte(output), &document); err != nil {
		t.Fatalf("the output is not JSON: %v; output: %s", err, output)
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
	code := runCLI([]string{"config", "print", "--format", "yaml"}, &stderr)
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
	if code := runCLI([]string{"config", "check", "--config", writeConfigFile(t, "backend = \"none\"\n")}, &stderr); code != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", code, stderr.String())
	}
}

// TestConfigCommandRejectsAnUnknownCommand covers the contract that an
// unknown configuration command is reported.
func TestConfigCommandRejectsAnUnknownCommand(t *testing.T) {
	var stderr strings.Builder
	code := runCLI([]string{"config", "explain"}, &stderr)
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
	code := runCLI([]string{}, &stderr)
	if code != 2 {
		t.Fatalf("run = %d, want 2; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "torrents directory") {
		t.Errorf("the report does not ask for a torrents directory: %s", stderr.String())
	}
}
