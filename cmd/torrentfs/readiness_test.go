package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionFlagPrintsBuildVersionAndSkipsTheTorrentsDirectory(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "torrentfs "+version {
		t.Fatalf("stdout = %q, want the build version", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestOutputReadinessRequiresConfiguredOutputs(t *testing.T) {
	// An HTTP-only deployment has no mount and no SMB listener, so it is ready.
	httpOnly := &outputReadiness{}
	if err := httpOnly.check(); err != nil {
		t.Fatalf("http-only check = %v, want nil", err)
	}

	// A configured mountpoint that never mounted is not ready.
	fuseOnly := &outputReadiness{}
	fuseOnly.fuseRequired.Store(true)
	if err := fuseOnly.check(); err == nil {
		t.Fatal("missing fuse mount reported ready")
	}
	fuseOnly.setFuseMounts(1)
	if err := fuseOnly.check(); err != nil {
		t.Fatalf("mounted fuse reported %v", err)
	}

	// SMB backend without a listening socket is not ready.
	smb := &outputReadiness{}
	smb.smbRequired.Store(true)
	if err := smb.check(); err == nil {
		t.Fatal("missing smb listener reported ready")
	}
	smb.setSMBListening(true)
	if err := smb.check(); err != nil {
		t.Fatalf("listening smb reported %v", err)
	}
	smb.setSMBListening(false)
	if err := smb.check(); err == nil {
		t.Fatal("stopped smb listener reported ready")
	}
}
