package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fakeSMBShutdown struct {
	recorder    *shutdownRecorder
	err         error
	hasDeadline bool
}

func (f *fakeSMBShutdown) Shutdown(ctx context.Context) error {
	f.recorder.record("smb")
	_, f.hasDeadline = ctx.Deadline()
	return f.err
}

func TestShutdownDrainsSMBBeforeSessionAndContinuesAfterError(t *testing.T) {
	recorder := &shutdownRecorder{}
	failed := errors.New("test SMB shutdown error")
	output := &fakeSMBShutdown{recorder: recorder, err: failed}
	sequence := shutdownSequence{smb: output, api: fakeAPIShutdown{recorder: recorder}, sess: fakeSessionCloser{recorder: recorder}}
	result := sequence.run()
	if !errors.Is(result.smb, failed) || result.close != nil || result.api != nil {
		t.Fatalf("shutdown result: %+v", result)
	}
	if !output.hasDeadline {
		t.Fatal("SMB shutdown had no deadline")
	}
	if got := recorder.recorded(); !reflect.DeepEqual(got, []string{"smb", "api", "close"}) {
		t.Fatalf("shutdown order=%v", got)
	}
}

func TestRunSMBDoesNotValidateOrMountUnusedFUSE(t *testing.T) {
	unsetConfigEnvironment(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	dir := t.TempDir()
	file := filepath.Join(t.TempDir(), "config.toml")
	text := fmt.Sprintf(`[filesystem]
backend = "smb"
[http]
listen_addr = ""
[fuse]
mountpoint = "/does-not-exist-and-must-not-be-mounted"
strm_mountpoint = "/also-not-used"
[smb]
listen_addr = %q
[connections]
listen_host = "127.0.0.1"
disable_ipv6 = true
bootstrap_nodes = ["127.0.0.1:1"]
`, listener.Addr().String())
	if err := os.WriteFile(file, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TORRENTFS_USERNAME", "SMB-login-is-not-a-Unix-user")
	t.Setenv("TORRENTFS_PASSWORD", "test-only-output-password")
	var stderr bytes.Buffer
	if code := run([]string{"-config", file, dir}, &stderr); code != 1 {
		t.Fatalf("exit=%d stderr=%s", code, &stderr)
	}
	if !strings.Contains(stderr.String(), "smb listen:") {
		t.Fatalf("unused FUSE settings interfered with SMB: %s", &stderr)
	}
	if strings.Contains(stderr.String(), "fuse mount failed") || strings.Contains(stderr.String(), "test-only-output-password") {
		t.Fatalf("unexpected startup output: %s", &stderr)
	}
}

func TestRunExplicitNoneRejectsAllOutputsDisabled(t *testing.T) {
	unsetConfigEnvironment(t)
	t.Setenv("TORRENTFS_FILESYSTEM_BACKEND", "none")
	t.Setenv("TORRENTFS_HTTP_LISTEN_ADDR", "")
	var stderr bytes.Buffer
	if code := run([]string{t.TempDir()}, &stderr); code != 2 {
		t.Fatalf("exit=%d stderr=%s", code, &stderr)
	}
}
