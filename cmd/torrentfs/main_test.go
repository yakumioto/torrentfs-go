package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/logging"
)

// shutdownRecorder records the order in which the shutdown stages ran and the
// errors each stage reports. A blocking Unmount stage records from its own
// goroutine, so every access is mutex-guarded.
type shutdownRecorder struct {
	mu    sync.Mutex
	order []string
}

func (r *shutdownRecorder) record(stage string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, stage)
}

func (r *shutdownRecorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

type fakeAPIShutdown struct {
	recorder *shutdownRecorder
	err      error
}

func (f fakeAPIShutdown) Shutdown(context.Context) error {
	f.recorder.record("api")
	return f.err
}

type fakeSessionCloser struct {
	recorder *shutdownRecorder
	err      error
}

func (f fakeSessionCloser) Close(context.Context) error {
	f.recorder.record("close")
	return f.err
}

type fakeMountUnmounter struct {
	recorder *shutdownRecorder
	err      error
}

func (f fakeMountUnmounter) Unmount() error {
	f.recorder.record("unmount")
	return f.err
}

// blockingMountUnmounter models a peer mount namespace holding a propagated
// copy: Unmount is entered, reports the stage, and then never returns until
// the test releases it.
type blockingMountUnmounter struct {
	recorder *shutdownRecorder
	entered  chan struct{}
	release  chan struct{}
}

func (f blockingMountUnmounter) Unmount() error {
	f.recorder.record("unmount")
	close(f.entered)
	<-f.release
	return nil
}

// countingMountUnmounter returns after a short delay and counts its calls.
type countingMountUnmounter struct {
	recorder *shutdownRecorder
	delay    time.Duration
	calls    atomic.Int32
}

func (f *countingMountUnmounter) Unmount() error {
	f.calls.Add(1)
	f.recorder.record("unmount")
	time.Sleep(f.delay)
	return nil
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
	if got, want := recorder.recorded(), []string{"api", "close", "unmount"}; !equalStrings(got, want) {
		t.Fatalf("shutdown order = %v, want %v", got, want)
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
	if got, want := recorder.recorded(), []string{"api", "close", "unmount"}; !equalStrings(got, want) {
		t.Fatalf("shutdown order = %v, want %v", got, want)
	}
	if !errors.Is(result.api, apiErr) || !errors.Is(result.close, closeErr) || !errors.Is(result.unmount, unmountErr) {
		t.Fatalf("shutdown errors = %+v, want api=%v close=%v unmount=%v", result, apiErr, closeErr, unmountErr)
	}
}

// TestShutdownSequenceSkipsAbsentStages covers the headless and mount-only
// configurations: a nil stage is not called and reports no error.
func TestShutdownSequenceReportsIncompleteWithoutStopped(t *testing.T) {
	var buf bytes.Buffer
	logger, _, err := logging.New(config.Log{Level: "info", Format: "text"}, &buf)
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	recorder := &shutdownRecorder{}
	sequence := shutdownSequence{
		logger: logger,
		api:    fakeAPIShutdown{recorder: recorder, err: errors.New("api shutdown failed")},
		sess:   fakeSessionCloser{recorder: recorder},
	}
	result := sequence.run()
	if result.api == nil {
		t.Fatal("shutdown result lost API failure")
	}
	output := buf.String()
	if strings.Contains(output, "msg=stopped") {
		t.Fatalf("failed shutdown reported stopped: %q", output)
	}
	if got := strings.Count(output, "shutdown incomplete"); got != 1 {
		t.Fatalf("shutdown incomplete records = %d, want 1: %q", got, output)
	}
	if !strings.Contains(output, "stage") || !strings.Contains(output, "http-api") {
		t.Fatalf("shutdown summary lacks failed stage: %q", output)
	}
}

func TestShutdownSequenceSkipsAbsentStages(t *testing.T) {
	recorder := &shutdownRecorder{}
	sequence := shutdownSequence{sess: fakeSessionCloser{recorder: recorder}}
	result := sequence.run()
	if got, want := recorder.recorded(), []string{"close"}; !equalStrings(got, want) {
		t.Fatalf("shutdown order = %v, want %v", got, want)
	}
	if result.api != nil || result.close != nil || result.unmount != nil {
		t.Fatalf("shutdown errors = %+v, want all nil", result)
	}
}

// TestShutdownSequenceUnmountTimeoutReturnsError pins the bounded wait: an
// Unmount that never returns (a peer mount namespace holding a propagated copy
// leaves it parked in the event-loop Wait) must not hang shutdown. The stage
// reports a timeout error that errors.Is recognises, the earlier stages still
// ran in order, and the process is expected to exit and reap the goroutine.
func TestShutdownSequenceUnmountTimeoutReturnsError(t *testing.T) {
	recorder := &shutdownRecorder{}
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	const timeout = 50 * time.Millisecond

	sequence := shutdownSequence{
		api:            fakeAPIShutdown{recorder: recorder},
		sess:           fakeSessionCloser{recorder: recorder},
		mount:          blockingMountUnmounter{recorder: recorder, entered: entered, release: release},
		unmountTimeout: timeout,
	}
	results := make(chan shutdownResult, 1)
	go func() { results <- sequence.run() }()

	var result shutdownResult
	select {
	case result = <-results:
	case <-time.After(20 * timeout):
		t.Fatalf("shutdown did not return within %s although unmountTimeout is %s", 20*timeout, timeout)
	}
	if !errors.Is(result.unmount, errUnmountTimeout) {
		t.Fatalf("unmount error = %v, want %v", result.unmount, errUnmountTimeout)
	}
	if result.api != nil || result.close != nil {
		t.Fatalf("shutdown errors = %+v, want api and close nil", result)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("unmount stage was never entered")
	}
	if got, want := recorder.recorded(), []string{"api", "close", "unmount"}; !equalStrings(got, want) {
		t.Fatalf("shutdown order = %v, want %v", got, want)
	}
}

// TestShutdownSequenceUnmountCompletesWithinTimeout covers the healthy path:
// an Unmount that returns before the deadline is not reported as a timeout and
// is called exactly once.
func TestShutdownSequenceUnmountCompletesWithinTimeout(t *testing.T) {
	recorder := &shutdownRecorder{}
	mount := &countingMountUnmounter{recorder: recorder, delay: 5 * time.Millisecond}
	sequence := shutdownSequence{
		api:            fakeAPIShutdown{recorder: recorder},
		sess:           fakeSessionCloser{recorder: recorder},
		mount:          mount,
		unmountTimeout: time.Second,
	}

	result := sequence.run()
	if result.api != nil || result.close != nil || result.unmount != nil {
		t.Fatalf("shutdown errors = %+v, want all nil", result)
	}
	if got, want := recorder.recorded(), []string{"api", "close", "unmount"}; !equalStrings(got, want) {
		t.Fatalf("shutdown order = %v, want %v", got, want)
	}
	if got := mount.calls.Load(); got != 1 {
		t.Fatalf("unmount calls = %d, want 1", got)
	}
}

// TestShutdownSequenceUnmountTimeoutDefaults checks that the override seam
// falls back to the package default instead of degrading into an immediate
// timeout or an unbounded wait.
func TestShutdownSequenceUnmountTimeoutDefaults(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{name: "zero uses default", timeout: 0, want: defaultUnmountTimeout},
		{name: "negative uses default", timeout: -time.Second, want: defaultUnmountTimeout},
		{name: "override wins", timeout: 250 * time.Millisecond, want: 250 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (shutdownSequence{unmountTimeout: tt.timeout}).effectiveUnmountTimeout(); got != tt.want {
				t.Fatalf("effectiveUnmountTimeout() = %s, want %s", got, tt.want)
			}
		})
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

func TestLoadConfigUsesEnvironmentWhenConfigPathIsOmitted(t *testing.T) {
	unsetConfigEnvironment(t)
	dataDir := filepath.Join(t.TempDir(), "environment-data")
	t.Setenv("TORRENTFS_PATHS_DATA_DIR", dataDir)

	cfg, err := loadConfig("", "", false)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Paths.DataDir != dataDir {
		t.Fatalf("DataDir = %q, want %q", cfg.Paths.DataDir, dataDir)
	}
}

func TestLoadConfigEnvironmentOverridesFile(t *testing.T) {
	unsetConfigEnvironment(t)
	fileDataDir := filepath.Join(t.TempDir(), "file-data")
	envDataDir := filepath.Join(t.TempDir(), "environment-data")
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[paths]\ndata_dir = "+strconv.Quote(fileDataDir)+"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("TORRENTFS_PATHS_DATA_DIR", envDataDir)

	cfg, err := loadConfig(path, "", false)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Paths.DataDir != envDataDir {
		t.Fatalf("DataDir = %q, want environment value %q", cfg.Paths.DataDir, envDataDir)
	}
}

func TestLoadConfigExplicitDataDirRemainsFinalOverride(t *testing.T) {
	unsetConfigEnvironment(t)
	fileDataDir := filepath.Join(t.TempDir(), "file-data")
	envDataDir := filepath.Join(t.TempDir(), "environment-data")
	cliDataDir := filepath.Join(t.TempDir(), "cli-data")
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[paths]\ndata_dir = "+strconv.Quote(fileDataDir)+"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("TORRENTFS_PATHS_DATA_DIR", envDataDir)

	cfg, err := loadConfig(path, cliDataDir, true)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Paths.DataDir != cliDataDir {
		t.Fatalf("DataDir = %q, want CLI value %q", cfg.Paths.DataDir, cliDataDir)
	}
}

func TestRunInvalidEnvironmentReturnsConfigurationExitCode(t *testing.T) {
	unsetConfigEnvironment(t)
	t.Setenv("TORRENTFS_CONNECTIONS_LISTEN_PORT", "invalid-run-port")
	var stderr bytes.Buffer

	code := run([]string{"-mountpoint", t.TempDir(), t.TempDir()}, &stderr)
	if code != 2 {
		t.Fatalf("run exit code = %d, want 2", code)
	}
	for _, want := range []string{"TORRENTFS_CONNECTIONS_LISTEN_PORT", "connections.listen_port"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	}
	if strings.Contains(stderr.String(), "invalid-run-port") {
		t.Fatalf("stderr = %q, must not contain raw environment value", stderr.String())
	}
}

func unsetConfigEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"TORRENTFS_PATHS_DATA_DIR",
		"TORRENTFS_PATHS_PAYLOAD_DIR",
		"TORRENTFS_CONNECTIONS_LISTEN_HOST",
		"TORRENTFS_CONNECTIONS_LISTEN_PORT",
		"TORRENTFS_PROXY_SOCKS5_URL",
		"TORRENTFS_CACHE_CAPACITY_BYTES",
		"TORRENTFS_IDENTITY_TRACKER_USER_AGENT",
		"TORRENTFS_IDENTITY_PEER_ID_PREFIX",
		"TORRENTFS_IDENTITY_EXTENDED_HANDSHAKE_CLIENT_VERSION",
		"TORRENTFS_HTTP_LISTEN_ADDR",
		"TORRENTFS_HTTP_MAX_UPLOAD_BYTES",
		"TORRENTFS_HTTP_AUTH_ENABLED",
		"TORRENTFS_HTTP_AUTH_USERNAME",
		"TORRENTFS_HTTP_AUTH_PASSWORD_HASH",
		"TORRENTFS_HTTP_AUTH_PASSWORD_HASH_FILE",
		"TORRENTFS_HTTP_AUTH_TOKEN_TTL",
		"TORRENTFS_LOG_LEVEL",
		"TORRENTFS_LOG_FORMAT",
		"TORRENTFS_LOG_ADD_SOURCE",
	} {
		name := name
		previous, wasSet := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
		t.Cleanup(func() {
			if wasSet {
				_ = os.Setenv(name, previous)
			} else {
				_ = os.Unsetenv(name)
			}
		})
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
