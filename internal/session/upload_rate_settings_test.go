package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

func writeSettingsFile(t *testing.T, metadataDir, body string) {
	t.Helper()
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		t.Fatalf("make metadata dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "upload_rate.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write settings file: %v", err)
	}
}

func TestLoadUploadRateSettingsWithoutFileIsUnlimited(t *testing.T) {
	settings, policy, err := loadUploadRateSettings(t.TempDir())
	if err != nil {
		t.Fatalf("loadUploadRateSettings: %v", err)
	}
	if settings.RateLimitBytesPerSecond != 0 || settings.Schedule != nil {
		t.Fatalf("settings = %+v, want disabled", settings)
	}
	if policy.enabled() || policy.scheduled {
		t.Fatalf("policy = %+v, want disabled", policy)
	}
	if got := policy.effectiveLimit(time.Now()); got != rate.Inf {
		t.Fatalf("effective limit = %v, want rate.Inf", got)
	}
}

func TestSaveAndLoadUploadRateSettings(t *testing.T) {
	metadataDir := t.TempDir()
	saved := UploadRateSettings{
		RateLimitBytesPerSecond: 1048576,
		Schedule:                &UploadRateSchedule{Start: "08:00", End: "22:00"},
	}
	published, err := saveUploadRateSettings(metadataDir, saved)
	if err != nil || !published {
		t.Fatalf("saveUploadRateSettings = (%t, %v), want committed", published, err)
	}

	raw, err := os.ReadFile(filepath.Join(metadataDir, "upload_rate.json"))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var record struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("decode sidecar: %v", err)
	}
	if record.Version != uploadRateSettingsVersion {
		t.Fatalf("sidecar version = %d, want %d", record.Version, uploadRateSettingsVersion)
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Fatal("sidecar is not newline terminated")
	}

	loaded, policy, err := loadUploadRateSettings(metadataDir)
	if err != nil {
		t.Fatalf("loadUploadRateSettings: %v", err)
	}
	if loaded.RateLimitBytesPerSecond != saved.RateLimitBytesPerSecond ||
		loaded.Schedule == nil || *loaded.Schedule != *saved.Schedule {
		t.Fatalf("loaded settings = %+v, want %+v", loaded, saved)
	}
	if !policy.scheduled || policy.limit != rate.Limit(saved.RateLimitBytesPerSecond) {
		t.Fatalf("loaded policy = %+v, want the saved window", policy)
	}
}

// TestLoadUploadRateSettingsRejectsUnusableFiles pins the fail-start contract:
// a sidecar the daemon cannot trust must not silently degrade to unlimited.
func TestLoadUploadRateSettingsRejectsUnusableFiles(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "unknown field", body: `{"version":1,"rate_limit_bytes_per_second":1024,"schedule":null,"extra":1}`},
		{name: "trailing data", body: "{\"version\":1,\"rate_limit_bytes_per_second\":1024,\"schedule\":null}{\"version\":1}"},
		{name: "unsupported version", body: `{"version":2,"rate_limit_bytes_per_second":1024,"schedule":null}`},
		{name: "missing version", body: `{"rate_limit_bytes_per_second":1024,"schedule":null}`},
		{name: "negative rate", body: `{"version":1,"rate_limit_bytes_per_second":-1,"schedule":null}`},
		{
			name: "schedule without a rate",
			body: `{"version":1,"rate_limit_bytes_per_second":0,"schedule":{"start":"08:00","end":"22:00"}}`,
		},
		{
			name: "reversed window",
			body: `{"version":1,"rate_limit_bytes_per_second":1024,"schedule":{"start":"22:00","end":"08:00"}}`,
		},
		{
			name: "loose time",
			body: `{"version":1,"rate_limit_bytes_per_second":1024,"schedule":{"start":"8:00","end":"22:00"}}`,
		},
		{name: "not json", body: "rate_limit_bytes_per_second = 1024\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadataDir := t.TempDir()
			writeSettingsFile(t, metadataDir, tt.body)
			if _, _, err := loadUploadRateSettings(metadataDir); err == nil {
				t.Fatal("loadUploadRateSettings accepted an unusable sidecar")
			} else if !strings.Contains(err.Error(), "upload_rate.json") {
				t.Fatalf("error %q does not name the sidecar", err)
			}
		})
	}
}

func TestLoadUploadRateSettingsRejectsNonRegularFile(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		metadataDir := t.TempDir()
		target := filepath.Join(t.TempDir(), "target.json")
		if err := os.WriteFile(target, []byte(`{"version":1,"rate_limit_bytes_per_second":1024,"schedule":null}`), 0o600); err != nil {
			t.Fatalf("write target: %v", err)
		}
		if err := os.Symlink(target, filepath.Join(metadataDir, "upload_rate.json")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if _, _, err := loadUploadRateSettings(metadataDir); err == nil {
			t.Fatal("loadUploadRateSettings followed a symlink")
		}
	})

	t.Run("directory", func(t *testing.T) {
		metadataDir := t.TempDir()
		if err := os.Mkdir(filepath.Join(metadataDir, "upload_rate.json"), 0o755); err != nil {
			t.Fatalf("make directory: %v", err)
		}
		if _, _, err := loadUploadRateSettings(metadataDir); err == nil {
			t.Fatal("loadUploadRateSettings accepted a directory")
		}
	})
}

// TestSaveUploadRateSettingsRefusesToReplaceNonRegularFile keeps the write path
// from clobbering something the daemon did not create.
func TestSaveUploadRateSettingsRefusesToReplaceNonRegularFile(t *testing.T) {
	metadataDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(metadataDir, "upload_rate.json"), 0o755); err != nil {
		t.Fatalf("make directory: %v", err)
	}
	published, err := saveUploadRateSettings(metadataDir, UploadRateSettings{RateLimitBytesPerSecond: 1024})
	if published || !errors.Is(err, ErrUploadRateSettingsStorageUnavailable) {
		t.Fatalf("saveUploadRateSettings = (%t, %v), want a pre-commit storage failure", published, err)
	}
}

// TestSaveUploadRateSettingsBeforeCommitFailureKeepsOldContent covers the
// publish-less failure: nothing changed on disk and the caller must keep its
// previous state.
func TestSaveUploadRateSettingsBeforeCommitFailureKeepsOldContent(t *testing.T) {
	metadataDir := t.TempDir()
	original := UploadRateSettings{RateLimitBytesPerSecond: 2048}
	if published, err := saveUploadRateSettings(metadataDir, original); err != nil || !published {
		t.Fatalf("seed save = (%t, %v)", published, err)
	}

	// A missing metadata directory makes the temporary file creation fail, which
	// happens before the rename that publishes anything.
	missing := filepath.Join(t.TempDir(), "absent")
	published, err := saveUploadRateSettings(missing, UploadRateSettings{RateLimitBytesPerSecond: 4096})
	if published {
		t.Fatal("saveUploadRateSettings reported a commit for a missing directory")
	}
	if !errors.Is(err, ErrUploadRateSettingsStorageUnavailable) {
		t.Fatalf("error = %v, want ErrUploadRateSettingsStorageUnavailable", err)
	}

	loaded, _, err := loadUploadRateSettings(metadataDir)
	if err != nil {
		t.Fatalf("loadUploadRateSettings: %v", err)
	}
	if loaded.RateLimitBytesPerSecond != original.RateLimitBytesPerSecond {
		t.Fatalf("loaded settings = %+v, want the untouched %+v", loaded, original)
	}
}

// TestSaveUploadRateSettingsAfterCommitFailureReportsUnconfirmedDurability
// covers the rename-then-sync-failure window: the new content is already visible
// to a restart, so the error must say the values were applied but durability is
// unconfirmed rather than pretend the save failed.
func TestSaveUploadRateSettingsAfterCommitFailureReportsUnconfirmedDurability(t *testing.T) {
	metadataDir := t.TempDir()
	restore := SetUploadRateDirSyncHookForTest(func(string) error {
		return errors.New("injected directory sync failure")
	})
	defer restore()

	next := UploadRateSettings{RateLimitBytesPerSecond: 8192}
	published, err := saveUploadRateSettings(metadataDir, next)
	if !published {
		t.Fatal("saveUploadRateSettings reported no commit after the rename")
	}
	if !errors.Is(err, ErrUploadRateSettingsDurabilityUnconfirmed) {
		t.Fatalf("error = %v, want ErrUploadRateSettingsDurabilityUnconfirmed", err)
	}

	restore()
	loaded, _, loadErr := loadUploadRateSettings(metadataDir)
	if loadErr != nil {
		t.Fatalf("loadUploadRateSettings: %v", loadErr)
	}
	if loaded.RateLimitBytesPerSecond != next.RateLimitBytesPerSecond {
		t.Fatalf("published content = %+v, want the new %+v", loaded, next)
	}
}

// TestSetUploadRateSettingsDurabilityUnconfirmedAppliesNewValue proves the
// session adopts the published value even when the directory sync failed: the
// running limit and the stored settings must match what a restart would read.
func TestSetUploadRateSettingsDurabilityUnconfirmedAppliesNewValue(t *testing.T) {
	torrentsDir := filepath.Join(t.TempDir(), "torrents")
	if err := os.MkdirAll(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	sess, err := New(config.Default(), torrentsDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	restore := SetUploadRateDirSyncHookForTest(func(string) error {
		return errors.New("injected directory sync failure")
	})
	defer restore()

	applied, err := sess.SetUploadRateSettings(context.Background(), UploadRateSettings{RateLimitBytesPerSecond: 16384})
	if !errors.Is(err, ErrUploadRateSettingsDurabilityUnconfirmed) {
		t.Fatalf("SetUploadRateSettings error = %v, want the durability-uncertain error", err)
	}
	if applied.RateLimitBytesPerSecond != 16384 {
		t.Fatalf("applied settings = %+v, want the new value", applied)
	}
	if got := sess.UploadRateSettings(); got.RateLimitBytesPerSecond != 16384 {
		t.Fatalf("stored settings = %+v, want the new value", got)
	}
	if limit, _ := sess.UploadRateLimitForTest(); limit != rate.Limit(16384) {
		t.Fatalf("running limit = %v, want 16384", limit)
	}

	restored, _, err := loadUploadRateSettings(sess.metadataDir)
	if err != nil {
		t.Fatalf("loadUploadRateSettings: %v", err)
	}
	if restored.RateLimitBytesPerSecond != 16384 {
		t.Fatalf("published settings = %+v, want the new value", restored)
	}
}

// TestSetUploadRateSettingsConcurrentUpdatesAndClose drives the update path
// from several goroutines while the session closes, and then proves the stored
// settings still match what a restart would read: the commit order is
// serialized by the session lock, so the file and memory cannot diverge.
func TestSetUploadRateSettingsConcurrentUpdatesAndClose(t *testing.T) {
	torrentsDir := filepath.Join(t.TempDir(), "torrents")
	if err := os.MkdirAll(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	sess, err := New(config.Default(), torrentsDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	metadataDir := sess.metadataDir

	const (
		writers   = 3
		perWriter = 12
	)
	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		failures []error
	)
	for writer := 0; writer < writers; writer++ {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			for attempt := 0; attempt < perWriter; attempt++ {
				_, err := sess.SetUploadRateSettings(context.Background(), UploadRateSettings{
					RateLimitBytesPerSecond: int64(1024 * (writer + 1) * (attempt + 1)),
				})
				if err != nil && !errors.Is(err, filesystem.ErrClosed) {
					errMu.Lock()
					failures = append(failures, err)
					errMu.Unlock()
					return
				}
			}
		}(writer)
	}

	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()
	if len(failures) > 0 {
		t.Fatalf("concurrent updates failed: %v", failures)
	}

	if _, err := sess.SetUploadRateSettings(context.Background(), UploadRateSettings{
		RateLimitBytesPerSecond: 4096,
	}); !errors.Is(err, filesystem.ErrClosed) {
		t.Fatalf("setter after Close = %v, want ErrClosed", err)
	}

	inMemory := sess.UploadRateSettings()
	onDisk, _, err := loadUploadRateSettings(metadataDir)
	if err != nil {
		t.Fatalf("loadUploadRateSettings: %v", err)
	}
	if inMemory.RateLimitBytesPerSecond != onDisk.RateLimitBytesPerSecond {
		t.Fatalf("memory %d and disk %d diverged after concurrent updates",
			inMemory.RateLimitBytesPerSecond, onDisk.RateLimitBytesPerSecond)
	}
}

func TestSaveUploadRateSettingsRejectsInvalidValues(t *testing.T) {
	metadataDir := t.TempDir()
	published, err := saveUploadRateSettings(metadataDir, UploadRateSettings{
		RateLimitBytesPerSecond: 1024,
		Schedule:                &UploadRateSchedule{Start: "23:00", End: "01:00"},
	})
	if published || !errors.Is(err, ErrInvalidUploadRateSettings) {
		t.Fatalf("saveUploadRateSettings = (%t, %v), want an invalid-settings rejection", published, err)
	}
	if _, statErr := os.Stat(filepath.Join(metadataDir, "upload_rate.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid settings still wrote a sidecar: %v", statErr)
	}
}
