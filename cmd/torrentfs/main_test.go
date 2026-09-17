package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// shutdownRecorder records the order in which the shutdown stages ran and the
// errors each stage reports.
type shutdownRecorder struct {
	order []string
}

type fakeAPIShutdown struct {
	recorder *shutdownRecorder
	err      error
}

func (f fakeAPIShutdown) Shutdown(context.Context) error {
	f.recorder.order = append(f.recorder.order, "api")
	return f.err
}

type fakeSessionCloser struct {
	recorder *shutdownRecorder
	err      error
}

func (f fakeSessionCloser) Close(context.Context) error {
	f.recorder.order = append(f.recorder.order, "close")
	return f.err
}

type fakeMountUnmounter struct {
	recorder *shutdownRecorder
	err      error
}

func (f fakeMountUnmounter) Unmount() error {
	f.recorder.order = append(f.recorder.order, "unmount")
	return f.err
}

// TestShutdownSequenceClosesSessionBeforeUnmount pins the production order:
// outstanding FUSE reads must be released by Session.Close before the mount is
// unmounted, or Unmount can block on them.
func TestShutdownSequenceClosesSessionBeforeUnmount(t *testing.T) {
	recorder := &shutdownRecorder{}
	sequence := shutdownSequence{
		api:   fakeAPIShutdown{recorder: recorder},
		sess:  fakeSessionCloser{recorder: recorder},
		mount: fakeMountUnmounter{recorder: recorder},
	}
	result := sequence.run()
	if want := []string{"api", "close", "unmount"}; !equalStrings(recorder.order, want) {
		t.Fatalf("shutdown order = %v, want %v", recorder.order, want)
	}
	if result.api != nil || result.close != nil || result.unmount != nil {
		t.Fatalf("shutdown errors = %+v, want all nil", result)
	}
}

// TestShutdownSequenceRunsEveryStageDespiteErrors checks that a failing stage
// never skips the stages after it and that every error is preserved.
func TestShutdownSequenceRunsEveryStageDespiteErrors(t *testing.T) {
	apiErr := errors.New("api shutdown failed")
	closeErr := errors.New("session close failed")
	unmountErr := errors.New("unmount failed")
	recorder := &shutdownRecorder{}
	sequence := shutdownSequence{
		api:   fakeAPIShutdown{recorder: recorder, err: apiErr},
		sess:  fakeSessionCloser{recorder: recorder, err: closeErr},
		mount: fakeMountUnmounter{recorder: recorder, err: unmountErr},
	}
	result := sequence.run()
	if want := []string{"api", "close", "unmount"}; !equalStrings(recorder.order, want) {
		t.Fatalf("shutdown order = %v, want %v", recorder.order, want)
	}
	if !errors.Is(result.api, apiErr) || !errors.Is(result.close, closeErr) || !errors.Is(result.unmount, unmountErr) {
		t.Fatalf("shutdown errors = %+v, want api=%v close=%v unmount=%v", result, apiErr, closeErr, unmountErr)
	}
}

// TestShutdownSequenceSkipsAbsentStages covers the headless and mount-only
// configurations: a nil stage is not called and reports no error.
func TestShutdownSequenceSkipsAbsentStages(t *testing.T) {
	recorder := &shutdownRecorder{}
	sequence := shutdownSequence{sess: fakeSessionCloser{recorder: recorder}}
	result := sequence.run()
	if want := []string{"close"}; !equalStrings(recorder.order, want) {
		t.Fatalf("shutdown order = %v, want %v", recorder.order, want)
	}
	if result.api != nil || result.close != nil || result.unmount != nil {
		t.Fatalf("shutdown errors = %+v, want all nil", result)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestRunMissingConfigReturnsConfigurationExitCode(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.toml")
	var stderr bytes.Buffer

	code := run([]string{
		"-mountpoint", t.TempDir(),
		"-config", missing,
		t.TempDir(),
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
		t.TempDir(),
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

func TestRunRejectsInvalidAuthenticationMaterialBeforeMount(t *testing.T) {
	work := t.TempDir()
	configPath := filepath.Join(work, "config.toml")
	configBody := "[paths]\ndata_dir = " + strconv.Quote(filepath.Join(work, "data")) + "\n\n" +
		"[http]\nlisten_addr = \"127.0.0.1:8080\"\n\n" +
		"[http.auth]\nenabled = true\nusername = \"alice\"\npassword_hash = \"not-a-bcrypt-hash\"\n"
	if err := os.WriteFile(configPath, []byte(configBody), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	torrentsDir := filepath.Join(work, "torrents")
	if err := os.Mkdir(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	var stderr bytes.Buffer
	code := run([]string{"-config", configPath, torrentsDir}, &stderr)
	if code != 2 {
		t.Fatalf("run exit code = %d, want 2; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "password_hash") {
		t.Fatalf("stderr = %q, want password_hash error", stderr.String())
	}
}

func TestRunRequiresExactlyOneTorrentsDirectory(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing",
			args: nil,
			want: "exactly one existing torrents directory",
		},
		{
			name: "multiple",
			args: []string{t.TempDir(), t.TempDir()},
			want: "exactly one existing torrents directory",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			args := append([]string{"-mountpoint", t.TempDir()}, tt.args...)
			if code := run(args, &stderr); code != 2 {
				t.Fatalf("run exit code = %d, want 2; stderr = %q", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tt.want)
			}
		})
	}
}

func TestRunRejectsNonDirectoryTorrentInput(t *testing.T) {
	file := filepath.Join(t.TempDir(), "input.torrent")
	if err := os.WriteFile(file, []byte("not a torrent"), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	var stderr bytes.Buffer

	code := run([]string{"-mountpoint", t.TempDir(), file}, &stderr)
	if code != 2 {
		t.Fatalf("run exit code = %d, want 2", code)
	}
	for _, want := range []string{file, "existing directory"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	}
}

func TestRunRejectsMissingTorrentDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-torrents")
	var stderr bytes.Buffer

	code := run([]string{"-mountpoint", t.TempDir(), missing}, &stderr)
	if code != 2 {
		t.Fatalf("run exit code = %d, want 2", code)
	}
	for _, want := range []string{missing, "existing directory", "no such file or directory"} {
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

func TestValidateTorrentDirRejectsSymlink(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "torrents")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("make symlink: %v", err)
	}

	err := validateTorrentDir(link)
	if err == nil || !strings.Contains(err.Error(), "existing directory") {
		t.Fatalf("validateTorrentDir(%q) = %v, want existing-directory error", link, err)
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
		t.Fatalf("stderr = %q, want torrent path", stderr.String())
	}
}
