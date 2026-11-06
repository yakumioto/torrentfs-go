package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// busyAddress writes a configuration file whose HTTP listener cannot bind,
// so the process exits after the startup output. The startup output is what
// the credential warning is part of.
func busyAddressConfig(t *testing.T, password string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	path := filepath.Join(t.TempDir(), "config.toml")
	body := fmt.Sprintf("[http]\nlisten_addr = %q\n", listener.Addr().String())
	if password != "" {
		body = fmt.Sprintf("password = %q\n", password) + body
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestRunWarnsAboutAPasswordFromTheConfigurationFile covers the credential
// warning: a password written in the configuration file stays readable on
// disk, so the startup warns about it. The warning never names the password.
func TestRunWarnsAboutAPasswordFromTheConfigurationFile(t *testing.T) {
	unsetConfigEnvironment(t)
	const secret = "a-file-password-that-must-not-be-logged"

	var stderr bytes.Buffer
	if code := run([]string{"-config", busyAddressConfig(t, secret), t.TempDir()}, &stderr); code != 1 {
		t.Fatalf("run exit code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "password") {
		t.Fatalf("stderr = %q, want the plaintext password warning", stderr.String())
	}
	if strings.Contains(stderr.String(), secret) {
		t.Fatalf("stderr = %q, must not contain the password", stderr.String())
	}
}

// TestRunDoesNotWarnAboutAPasswordFromTheEnvironment covers the other half of
// the warning: a password the environment supplied is not on disk, so the
// startup stays quiet.
func TestRunDoesNotWarnAboutAPasswordFromTheEnvironment(t *testing.T) {
	unsetConfigEnvironment(t)
	const secret = "an-environment-password-that-must-not-be-logged"
	t.Setenv("TORRENTFS_PASSWORD", secret)

	var stderr bytes.Buffer
	if code := run([]string{"-config", busyAddressConfig(t, ""), t.TempDir()}, &stderr); code != 1 {
		t.Fatalf("run exit code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "readable on disk") {
		t.Fatalf("stderr = %q, want no plaintext password warning", stderr.String())
	}
	if strings.Contains(stderr.String(), secret) {
		t.Fatalf("stderr = %q, must not contain the password", stderr.String())
	}
}

// TestRunDoesNotWarnWithoutAPassword covers the quiet case: a deployment
// without credentials has nothing to warn about.
func TestRunDoesNotWarnWithoutAPassword(t *testing.T) {
	unsetConfigEnvironment(t)

	var stderr bytes.Buffer
	if code := run([]string{"-config", busyAddressConfig(t, ""), t.TempDir()}, &stderr); code != 1 {
		t.Fatalf("run exit code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "readable on disk") {
		t.Fatalf("stderr = %q, want no plaintext password warning", stderr.String())
	}
}
