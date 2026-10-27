package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

type fakeSMBShutdown struct {
	recorder *shutdownRecorder
	err      error
}

func (s fakeSMBShutdown) Shutdown() error { s.recorder.record("smb"); return s.err }

func TestShutdownDrainsSMBBeforeSessionDespiteFailure(t *testing.T) {
	r := &shutdownRecorder{}
	failure := errors.New("smb failed")
	result := (shutdownSequence{api: fakeAPIShutdown{recorder: r}, smb: fakeSMBShutdown{recorder: r, err: failure},
		sess: fakeSessionCloser{recorder: r}, mounts: []mountedFilesystem{{server: fakeMountUnmounter{recorder: r}}}}).run()
	if !equalStrings(r.recorded(), []string{"api", "smb", "close", "unmount"}) || !errors.Is(result.smb, failure) {
		t.Fatalf("shutdown: %v %+v", r.recorded(), result)
	}
}

func TestSMBModeRejectsFUSEFlagsBeforeMountValidation(t *testing.T) {
	t.Setenv("TORRENTFS_FILESYSTEM_MODE", "smb")
	t.Setenv("TORRENTFS_USERNAME", "reader")
	t.Setenv("TORRENTFS_PASSWORD", "secret")
	t.Setenv("TORRENTFS_HTTP_LISTEN_ADDR", "")
	for _, flag := range []string{"-mountpoint", "-strm-mountpoint"} {
		var stderr bytes.Buffer
		code := run([]string{flag, "/nonexistent/fuse/mount", t.TempDir()}, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), "SMB mode does not accept") {
			t.Fatalf("%s: code=%d %s", flag, code, stderr.String())
		}
	}
}

func TestSMBBindFailureAndImmediateShutdown(t *testing.T) {
	cfg := config.Default()
	cfg.Filesystem.Mode = "smb"
	cfg.SMB.Username, cfg.SMB.Password = "reader", "secret"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.SMB.ListenAddr = ln.Addr().String()
	if _, err := startSMB(context.Background(), cfg, nil, nil); err == nil {
		t.Fatal("occupied address accepted")
	}
	_ = ln.Close()
	cfg.SMB.ListenAddr = "127.0.0.1:0"
	for range 10 {
		srv, err := startSMB(context.Background(), cfg, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.Shutdown(); err != nil {
			t.Fatal(err)
		}
	}
}
