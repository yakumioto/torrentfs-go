package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
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
		api:    fakeAPIShutdown{recorder: recorder},
		sess:   fakeSessionCloser{recorder: recorder},
		mounts: []mountedFilesystem{{mountpoint: "/files", server: fakeMountUnmounter{recorder: recorder}}},
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
		api:    fakeAPIShutdown{recorder: recorder, err: apiErr},
		sess:   fakeSessionCloser{recorder: recorder, err: closeErr},
		mounts: []mountedFilesystem{{mountpoint: "/files", server: fakeMountUnmounter{recorder: recorder, err: unmountErr}}},
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
		mounts:         []mountedFilesystem{{mountpoint: "/files", server: blockingMountUnmounter{recorder: recorder, entered: entered, release: release}}},
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
		mounts:         []mountedFilesystem{{mountpoint: "/files", server: mount}},
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

func TestShutdownSequenceDualMountsPreservesEveryError(t *testing.T) {
	apiErr := errors.New("api shutdown failed")
	closeErr := errors.New("session close failed")
	filesErr := errors.New("files unmount failed")
	strmErr := errors.New("STRM unmount failed")
	recorder := &shutdownRecorder{}
	sequence := shutdownSequence{
		api:  fakeAPIShutdown{recorder: recorder, err: apiErr},
		sess: fakeSessionCloser{recorder: recorder, err: closeErr},
		mounts: []mountedFilesystem{
			{mountpoint: "/files", server: fakeMountUnmounter{recorder: recorder, err: filesErr}},
			{mountpoint: "/strm", server: fakeMountUnmounter{recorder: recorder, err: strmErr}},
		},
	}
	result := sequence.run()
	if got, want := recorder.recorded(), []string{"api", "close", "unmount", "unmount"}; !equalStrings(got, want) {
		t.Fatalf("shutdown order = %v, want %v", got, want)
	}
	if !errors.Is(result.api, apiErr) || !errors.Is(result.close, closeErr) ||
		!errors.Is(result.unmount, filesErr) || !errors.Is(result.unmount, strmErr) {
		t.Fatalf("shutdown errors lost an original failure: %+v", result)
	}
	for _, path := range []string{"/files", "/strm"} {
		if !strings.Contains(result.unmount.Error(), path) {
			t.Fatalf("unmount error = %v, want path %s", result.unmount, path)
		}
	}
}

func TestShutdownSequenceDualMountsShareUnmountDeadline(t *testing.T) {
	recorder := &shutdownRecorder{}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	entered := []chan struct{}{make(chan struct{}), make(chan struct{})}
	const timeout = 200 * time.Millisecond
	sequence := shutdownSequence{
		api:  fakeAPIShutdown{recorder: recorder},
		sess: fakeSessionCloser{recorder: recorder},
		mounts: []mountedFilesystem{
			{mountpoint: "/files", server: blockingMountUnmounter{recorder: recorder, entered: entered[0], release: release}},
			{mountpoint: "/strm", server: blockingMountUnmounter{recorder: recorder, entered: entered[1], release: release}},
		},
		unmountTimeout: timeout,
	}
	started := time.Now()
	results := make(chan shutdownResult, 1)
	go func() { results <- sequence.run() }()
	for _, entry := range entered {
		select {
		case <-entry:
		case <-results:
			t.Fatal("shutdown returned before both unmounts were started")
		case <-time.After(5 * timeout):
			t.Fatal("both unmounts were not started concurrently")
		}
	}
	var result shutdownResult
	select {
	case result = <-results:
	case <-time.After(5 * timeout):
		t.Fatal("dual-mount shutdown did not return at its shared deadline")
	}
	if elapsed := time.Since(started); elapsed >= 2*timeout {
		t.Fatalf("unmount stage took %s, want one shared %s deadline", elapsed, timeout)
	}
	if !errors.Is(result.unmount, errUnmountTimeout) || result.api != nil || result.close != nil {
		t.Fatalf("shutdown errors = %+v, want only unmount timeouts", result)
	}
	if got := strings.Count(result.unmount.Error(), errUnmountTimeout.Error()); got != 2 {
		t.Fatalf("timeout reports = %d, want 2: %v", got, result.unmount)
	}
	for _, path := range []string{"/files", "/strm"} {
		if !strings.Contains(result.unmount.Error(), path) {
			t.Fatalf("timeout error = %v, want path %s", result.unmount, path)
		}
	}
	if got, want := recorder.recorded(), []string{"api", "close", "unmount", "unmount"}; !equalStrings(got, want) {
		t.Fatalf("shutdown order = %v, want %v", got, want)
	}
}

func TestShutdownSequenceDualMountFailureDoesNotSkipTimeout(t *testing.T) {
	for _, failingIndex := range []int{0, 1} {
		t.Run([]string{"files fails", "STRM fails"}[failingIndex], func(t *testing.T) {
			recorder := &shutdownRecorder{}
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			unmountErr := errors.New("unmount failed")
			mounts := []mountedFilesystem{{mountpoint: "/files"}, {mountpoint: "/strm"}}
			mounts[failingIndex].server = fakeMountUnmounter{recorder: recorder, err: unmountErr}
			mounts[1-failingIndex].server = blockingMountUnmounter{recorder: recorder, entered: make(chan struct{}), release: release}
			sequence := shutdownSequence{mounts: mounts, unmountTimeout: 30 * time.Millisecond}
			result := sequence.run()
			if !errors.Is(result.unmount, unmountErr) || !errors.Is(result.unmount, errUnmountTimeout) {
				t.Fatalf("unmount error = %v, want original failure and timeout", result.unmount)
			}
			if got, want := recorder.recorded(), []string{"unmount", "unmount"}; !equalStrings(got, want) {
				t.Fatalf("unmount calls = %v, want %v", got, want)
			}
		})
	}
}

// mountOnlyBackend supplies a distinct identity; the fake mounter never reads it.
type mountOnlyBackend struct{ filesystem.Backend }

func TestMountFilesystemsUsesIndependentOptionsForEachView(t *testing.T) {
	recorder := &shutdownRecorder{}
	backends := []filesystem.Backend{&mountOnlyBackend{}, &mountOnlyBackend{}}
	requests := []mountRequest{
		{mountpoint: "/files", backend: backends[0]},
		{mountpoint: "/strm", backend: backends[1]},
	}
	var optionsSeen []*fs.Options
	var paths []string
	mounts, err := mountFilesystems(requests, true, func(path string, backend filesystem.Backend, options *fs.Options) (mountUnmounter, error) {
		index := len(paths)
		paths = append(paths, path)
		if backend != backends[index] {
			t.Fatalf("mount %s received the wrong backend", path)
		}
		if options.UID != uint32(os.Getuid()) || options.GID != uint32(os.Getgid()) || !options.AllowOther {
			t.Fatalf("mount %s options = %+v, want current identity and allow_other", path, options)
		}
		if options.RootStableAttr != nil || options.SyncRead {
			t.Fatalf("mount %s reused options modified by a previous mount", path)
		}
		optionsSeen = append(optionsSeen, options)
		options.RootStableAttr = &fs.StableAttr{Ino: 42}
		options.SyncRead = true
		return fakeMountUnmounter{recorder: recorder}, nil
	})
	if err != nil {
		t.Fatalf("mountFilesystems: %v", err)
	}
	if got, want := paths, []string{"/files", "/strm"}; !equalStrings(got, want) {
		t.Fatalf("mount order = %v, want %v", got, want)
	}
	if len(mounts) != 2 || optionsSeen[0] == optionsSeen[1] {
		t.Fatalf("mounts = %+v, want two mounts with distinct options", mounts)
	}
	result := (shutdownSequence{mounts: mounts}).run()
	if result.unmount != nil {
		t.Fatalf("cleanup: %v", result.unmount)
	}
	if got, want := recorder.recorded(), []string{"unmount", "unmount"}; !equalStrings(got, want) {
		t.Fatalf("unmount calls = %v, want %v", got, want)
	}
}

func TestMountFilesystemsFailureRetainsStartedMountsForOrderedCleanup(t *testing.T) {
	for _, failingIndex := range []int{0, 1} {
		t.Run([]string{"files fails", "STRM fails"}[failingIndex], func(t *testing.T) {
			recorder := &shutdownRecorder{}
			mountErr := errors.New("mount failed")
			requests := []mountRequest{{mountpoint: "/files"}, {mountpoint: "/strm"}}
			calls := 0
			mounts, err := mountFilesystems(requests, false, func(_ string, _ filesystem.Backend, options *fs.Options) (mountUnmounter, error) {
				index := calls
				calls++
				if options.AllowOther {
					t.Fatal("mount unexpectedly enabled allow_other")
				}
				if index == failingIndex {
					return nil, mountErr
				}
				return fakeMountUnmounter{recorder: recorder}, nil
			})
			if !errors.Is(err, mountErr) || !strings.Contains(err.Error(), requests[failingIndex].mountpoint) {
				t.Fatalf("mount error = %v, want failed path and original error", err)
			}
			if len(mounts) != failingIndex || calls != failingIndex+1 {
				t.Fatalf("started mounts = %d, mount calls = %d, want %d and %d", len(mounts), calls, failingIndex, failingIndex+1)
			}
			sequence := shutdownSequence{
				api:    fakeAPIShutdown{recorder: recorder},
				sess:   fakeSessionCloser{recorder: recorder},
				mounts: mounts,
			}
			result := sequence.run()
			if result.api != nil || result.close != nil || result.unmount != nil {
				t.Fatalf("startup cleanup errors = %+v, want all nil", result)
			}
			want := []string{"api", "close"}
			if failingIndex == 1 {
				want = append(want, "unmount")
			}
			if got := recorder.recorded(); !equalStrings(got, want) {
				t.Fatalf("startup cleanup order = %v, want %v", got, want)
			}
		})
	}
}

func TestMountFilesystemsAllowsSTRMOnlyAndHeadless(t *testing.T) {
	for _, strmPath := range []string{"/strm", ""} {
		t.Run(strmPath, func(t *testing.T) {
			recorder := &shutdownRecorder{}
			var paths []string
			mounts, err := mountFilesystems([]mountRequest{{}, {mountpoint: strmPath}}, false,
				func(path string, _ filesystem.Backend, _ *fs.Options) (mountUnmounter, error) {
					paths = append(paths, path)
					return fakeMountUnmounter{recorder: recorder}, nil
				})
			if err != nil {
				t.Fatalf("mountFilesystems: %v", err)
			}
			wantCount := 0
			if strmPath != "" {
				wantCount = 1
			}
			if len(mounts) != wantCount || len(paths) != wantCount {
				t.Fatalf("mounts = %+v, calls = %v, want %d", mounts, paths, wantCount)
			}
			if wantCount != 0 && paths[0] != strmPath {
				t.Fatalf("mounted %s, want STRM path %s", paths[0], strmPath)
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
	if err := os.WriteFile(path, []byte("[connections]\nlisten_port = -1\n"), 0o644); err != nil {
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
	for _, want := range []string{path, "connections.listen_port"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	}
}

func TestRunRejectsInvalidAuthenticationMaterialBeforeMount(t *testing.T) {
	work := t.TempDir()
	configPath := filepath.Join(work, "config.toml")
	configBody := "[http]\nlisten_addr = \"127.0.0.1:8080\"\n\n" +
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

func TestRunRejectsRemovedDataDirFlag(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"-data-dir", t.TempDir(), "-mountpoint", t.TempDir(), t.TempDir()}, &stderr)
	if code != 2 {
		t.Fatalf("run exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Fatalf("stderr = %q, want unknown flag error", stderr.String())
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
		"TORRENTFS_PATHS_PAYLOAD_DIR",
		"TORRENTFS_CONNECTIONS_LISTEN_HOST",
		"TORRENTFS_CONNECTIONS_LISTEN_PORT",
		"TORRENTFS_MOUNT_ALLOW_OTHER",
		"TORRENTFS_MOUNT_READ_TIMEOUT",
		"TORRENTFS_FILESYSTEM_BACKEND",
		"TORRENTFS_FILESYSTEM_READ_TIMEOUT",
		"TORRENTFS_FUSE_MOUNTPOINT",
		"TORRENTFS_FUSE_STRM_MOUNTPOINT",
		"TORRENTFS_FUSE_ALLOW_OTHER",
		"TORRENTFS_SMB_ENABLED",
		"TORRENTFS_SMB_LISTEN_ADDR",
		"TORRENTFS_SMB_USERNAME",
		"TORRENTFS_SMB_PASSWORD_FILE",
		"TORRENTFS_SMB_STRM_ENABLED",
		"TORRENTFS_STRM_ENABLED",
		"TORRENTFS_PROXY_SOCKS5_URL",
		"TORRENTFS_CACHE_CAPACITY",
		"TORRENTFS_IDENTITY_TRACKER_USER_AGENT",
		"TORRENTFS_IDENTITY_PEER_ID_PREFIX",
		"TORRENTFS_IDENTITY_EXTENDED_HANDSHAKE_CLIENT_VERSION",
		"TORRENTFS_HTTP_LISTEN_ADDR",
		"TORRENTFS_STRM_BASE_URL",
		"TORRENTFS_HTTP_MAX_UPLOAD_SIZE",
		"TORRENTFS_HTTP_AUTH_ENABLED",
		"TORRENTFS_USERNAME",
		"TORRENTFS_PASSWORD",
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

func TestValidateMountpointsRejectsOverlapsAndSymlinkAliases(t *testing.T) {
	root := t.TempDir()
	files := filepath.Join(root, "files")
	strm := filepath.Join(root, "strm")
	for _, path := range []string{filepath.Join(files, "child"), filepath.Join(strm, "child")} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("make mountpoint: %v", err)
		}
	}
	filesAlias := filepath.Join(root, "files-alias")
	childAlias := filepath.Join(root, "child-alias")
	for link, target := range map[string]string{filesAlias: files, childAlias: filepath.Join(files, "child")} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("make mountpoint alias: %v", err)
		}
	}
	tests := []struct {
		name  string
		files string
		strm  string
	}{
		{name: "same", files: files, strm: files},
		{name: "normalized same", files: files + "/./", strm: files},
		{name: "STRM inside files", files: files, strm: filepath.Join(files, "child")},
		{name: "files inside STRM", files: filepath.Join(strm, "child"), strm: strm},
		{name: "same through symlink", files: files, strm: filesAlias},
		{name: "STRM inside files through symlink", files: filesAlias, strm: childAlias},
		{name: "files inside STRM through symlink", files: childAlias, strm: filesAlias},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := validateMountpoints(tt.files, tt.strm, "http://127.0.0.1:8080")
			if err == nil || !strings.Contains(err.Error(), "must not be the same or nested") {
				t.Fatalf("validateMountpoints(%q, %q) = %v, want overlap error", tt.files, tt.strm, err)
			}
		})
	}
}

func TestValidateMountpointsCanonicalizesIndependentViews(t *testing.T) {
	root := t.TempDir()
	files := filepath.Join(root, "share")
	strm := filepath.Join(root, "share-strm")
	for _, path := range []string{files, strm} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatalf("make mountpoint: %v", err)
		}
	}
	alias := filepath.Join(root, "strm-alias")
	if err := os.Symlink(strm, alias); err != nil {
		t.Fatalf("make mountpoint alias: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	relativeFiles, err := filepath.Rel(cwd, files)
	if err != nil {
		t.Fatalf("relative mountpoint: %v", err)
	}
	gotFiles, gotSTRM, err := validateMountpoints(relativeFiles, alias, "http://127.0.0.1:8080")
	if err != nil || gotFiles != files || gotSTRM != strm {
		t.Fatalf("validateMountpoints = (%q, %q, %v), want (%q, %q, nil)", gotFiles, gotSTRM, err, files, strm)
	}
	gotFiles, gotSTRM, err = validateMountpoints("", alias, "http://127.0.0.1:8080")
	if err != nil || gotFiles != "" || gotSTRM != strm {
		t.Fatalf("STRM-only validation = (%q, %q, %v), want (%q, %q, nil)", gotFiles, gotSTRM, err, "", strm)
	}
}

func TestValidateMountpointsRequiresBaseURLAndDirectoriesOnlyWhenEnabled(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatalf("write non-directory mountpoint: %v", err)
	}
	_, _, err := validateMountpoints("", t.TempDir(), "")
	if err == nil || !strings.Contains(err.Error(), "strm.base_url") {
		t.Fatalf("missing base URL error = %v", err)
	}
	for _, tt := range []struct{ files, strm string }{
		{files: "", strm: missing},
		{files: "", strm: file},
		{files: missing, strm: t.TempDir()},
		{files: file, strm: t.TempDir()},
	} {
		_, _, err := validateMountpoints(tt.files, tt.strm, "http://127.0.0.1:8080")
		if err == nil || !strings.Contains(err.Error(), "existing directory") {
			t.Fatalf("validateMountpoints(%q, %q) = %v, want directory error", tt.files, tt.strm, err)
		}
	}
	for _, original := range []string{"", missing, file, "relative/../mount"} {
		files, strm, err := validateMountpoints(original, "", "")
		if err != nil || files != original || strm != "" {
			t.Fatalf("disabled STRM changed existing behavior: (%q, %q, %v)", files, strm, err)
		}
	}
}

func TestRunRejectsSTRMConfigurationBeforeStartup(t *testing.T) {
	for _, tt := range []struct {
		name      string
		baseURL   string
		httpAddr  string
		overlap   bool
		wantError string
	}{
		{name: "missing base URL", httpAddr: "127.0.0.1:8080", wantError: "strm.base_url"},
		{name: "HTTP disabled", baseURL: "http://127.0.0.1:8080", wantError: "strm.base_url"},
		{name: "userinfo", baseURL: "http://user:strm-sensitive@127.0.0.1:8080", httpAddr: "127.0.0.1:8080", wantError: "strm.base_url"},
		{name: "overlapping mounts", baseURL: "http://127.0.0.1:8080", httpAddr: "127.0.0.1:8080", overlap: true, wantError: "must not be the same or nested"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			unsetConfigEnvironment(t)
			t.Setenv("TORRENTFS_STRM_BASE_URL", tt.baseURL)
			t.Setenv("TORRENTFS_HTTP_LISTEN_ADDR", tt.httpAddr)
			torrents := t.TempDir()
			strm := t.TempDir()
			args := []string{"-strm-mountpoint", strm}
			if tt.overlap {
				args = append(args, "-mountpoint", strm)
			}
			var stderr bytes.Buffer
			if code := run(append(args, torrents), &stderr); code != 2 {
				t.Fatalf("run exit code = %d, want 2; stderr = %q", code, stderr.String())
			}
			output := stderr.String()
			if !strings.Contains(output, tt.wantError) {
				t.Fatalf("stderr = %q, want %q", output, tt.wantError)
			}
			if strings.Contains(output, "torrentfs starting") || strings.Contains(output, "strm-sensitive") {
				t.Fatalf("configuration error reached startup or exposed credentials: %q", output)
			}
			if _, err := os.Stat(filepath.Join(torrents, ".metadata")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("configuration error initialized session metadata: %v", err)
			}
		})
	}
}

func TestRunRecognizesSTRMMountpointFlagForms(t *testing.T) {
	for _, flagName := range []string{"-strm-mountpoint", "--strm-mountpoint", "-strm-mountpoint=", "--strm-mountpoint="} {
		t.Run(flagName, func(t *testing.T) {
			unsetConfigEnvironment(t)
			mountpoint := t.TempDir()
			args := []string{flagName, mountpoint}
			if strings.HasSuffix(flagName, "=") {
				args = []string{flagName + mountpoint}
			}
			var stderr bytes.Buffer
			if code := run(append(args, t.TempDir()), &stderr); code != 2 {
				t.Fatalf("run exit code = %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), "strm.base_url") {
				t.Fatalf("stderr = %q, want recognized STRM flag and missing base URL", stderr.String())
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

func TestUsageDescribesAPIOnlyTorrentManagement(t *testing.T) {
	if strings.Contains(usageText, "scanned "+"at startup") || strings.Contains(usageText, "while "+"running") {
		t.Fatalf("usage still describes directory scanning: %q", usageText)
	}
	for _, want := range []string{"HTTP API", "root .torrent files", "ignored", "Canonical metainfo", "<torrents-dir>/<infohash>.torrent", ".metadata directory", "-strm-mountpoint", "strm.base_url", "mounted alone", "native SMB", "HTTP-only default", "SMB can run without HTTP", "symbolic-link aliases", "no metadata/ or stats/ control directory"} {
		if !strings.Contains(usageText, want) {
			t.Fatalf("usage = %q, want %q", usageText, want)
		}
	}
	if strings.Contains(usageText, "Durable metainfo") {
		t.Fatalf("usage still ambiguously places metainfo in .metadata: %q", usageText)
	}
}
