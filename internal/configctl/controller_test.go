package configctl_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/configctl"
)

// writeConfigFile writes a configuration file into a temporary directory and
// returns its path. The directory is private to the test, so a permission
// change cannot leak into another case.
func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "torrentfs.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write configuration file: %v", err)
	}
	return path
}

// keyByPath returns the view of one configuration key.
func keyByPath(t *testing.T, view configctl.View, path string) configctl.Key {
	t.Helper()
	for _, key := range view.Keys {
		if key.Path == path {
			return key
		}
	}
	t.Fatalf("the configuration view has no key %q", path)
	return configctl.Key{}
}

func loadView(t *testing.T, path string) configctl.View {
	t.Helper()
	cfg, sources, err := config.LoadWithSources(path)
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}
	view, err := configctl.New(path, cfg, sources).View()
	if err != nil {
		t.Fatalf("configuration view: %v", err)
	}
	return view
}

// TestViewDescribesEveryConfigurationKey covers the contract of the
// configuration page: every configuration key carries its schema metadata, the
// value the file layer holds, the effective value, and where that value came
// from.
func TestViewDescribesEveryConfigurationKey(t *testing.T) {
	path := writeConfigFile(t, "backend = \"none\"\npassword = \"s3cr3t\"\n\n[log]\nlevel = \"info\"\n")
	t.Setenv("TORRENTFS_LOG_LEVEL", "debug")

	view := loadView(t, path)

	if view.ConfigFile != path {
		t.Errorf("config file = %q, want %q", view.ConfigFile, path)
	}
	if view.ReadOnlyReason != "" {
		t.Errorf("read-only reason = %q, want empty for a writable configuration file", view.ReadOnlyReason)
	}
	if got, want := len(view.Keys), len(config.Schema()); got != want {
		t.Fatalf("key count = %d, want %d", got, want)
	}

	level := keyByPath(t, view, "log.level")
	if level.Section != "log" {
		t.Errorf("log.level section = %q, want log", level.Section)
	}
	if level.Kind != "string" {
		t.Errorf("log.level kind = %q, want string", level.Kind)
	}
	if len(level.Values) != 0 {
		t.Errorf("log.level values = %v, want none", level.Values)
	}
	if level.Default != "info" {
		t.Errorf("log.level default = %q, want info", level.Default)
	}
	if level.FileValue != "info" {
		t.Errorf("log.level file value = %q, want info", level.FileValue)
	}
	if level.EffectiveValue != "debug" {
		t.Errorf("log.level effective value = %q, want debug", level.EffectiveValue)
	}
	if level.Source != string(config.SourceEnvironment) {
		t.Errorf("log.level source = %q, want environment", level.Source)
	}
	if level.ReadOnlyReason != configctl.ReadOnlyEnvironmentOverride {
		t.Errorf("log.level read-only reason = %q, want %q", level.ReadOnlyReason, configctl.ReadOnlyEnvironmentOverride)
	}
	if level.Environment != "TORRENTFS_LOG_LEVEL" {
		t.Errorf("log.level environment = %q, want TORRENTFS_LOG_LEVEL", level.Environment)
	}
	if level.Component != "logging" {
		t.Errorf("log.level component = %q, want logging", level.Component)
	}
	if !level.HotApply {
		t.Error("log.level reports HotApply=false, want true")
	}
	if level.Documentation == "" {
		t.Error("log.level has no documentation")
	}

	backend := keyByPath(t, view, "backend")
	if backend.Section != "" {
		t.Errorf("backend section = %q, want the root section", backend.Section)
	}
	if backend.Kind != "enum" {
		t.Errorf("backend kind = %q, want enum", backend.Kind)
	}
	if len(backend.Values) != 3 || backend.Values[0] != "none" || backend.Values[2] != "smb" {
		t.Errorf("backend values = %v, want none, fuse, smb", backend.Values)
	}
	if backend.Source != string(config.SourceFile) || backend.FileValue != "none" || backend.EffectiveValue != "none" {
		t.Errorf("backend = source %q, file %q, effective %q; want file/none/none",
			backend.Source, backend.FileValue, backend.EffectiveValue)
	}
	if backend.HotApply {
		t.Error("backend reports HotApply=true, want false")
	}
	if backend.ReadOnlyReason != "" {
		t.Errorf("backend read-only reason = %q, want empty", backend.ReadOnlyReason)
	}

	// A default value and its source are visible even when no input set the
	// key, so the page can explain the value of every key.
	capacity := keyByPath(t, view, "cache.capacity")
	if capacity.Source != string(config.SourceDefault) {
		t.Errorf("cache.capacity source = %q, want default", capacity.Source)
	}
	if capacity.FileValue != capacity.Default || capacity.EffectiveValue != capacity.Default {
		t.Errorf("cache.capacity values = file %q, effective %q, default %q; want the default throughout",
			capacity.FileValue, capacity.EffectiveValue, capacity.Default)
	}

	// The password is a secret: it is readable nowhere and the page cannot
	// edit it, but the metadata still tells a reader why.
	password := keyByPath(t, view, "password")
	if !password.Secret {
		t.Error("password reports Secret=false, want true")
	}
	if password.ReadOnlyReason != configctl.ReadOnlySecret {
		t.Errorf("password read-only reason = %q, want %q", password.ReadOnlyReason, configctl.ReadOnlySecret)
	}
	if password.FileValue != "" || password.EffectiveValue != "" {
		t.Errorf("password values leaked: file %q, effective %q", password.FileValue, password.EffectiveValue)
	}
}

// TestViewWithoutConfigurationFileIsReadOnly covers a deployment that starts
// without -config: the page shows the defaults and explains how to enable
// editing.
func TestViewWithoutConfigurationFileIsReadOnly(t *testing.T) {
	view, err := configctl.New("", config.Default(), config.Sources{}).View()
	if err != nil {
		t.Fatalf("configuration view: %v", err)
	}

	if view.ConfigFile != "" {
		t.Errorf("config file = %q, want empty", view.ConfigFile)
	}
	if view.ReadOnlyReason != configctl.ReadOnlyNotConfigured {
		t.Fatalf("read-only reason = %q, want %q", view.ReadOnlyReason, configctl.ReadOnlyNotConfigured)
	}
	if len(view.Keys) != len(config.Schema()) {
		t.Fatalf("key count = %d, want %d", len(view.Keys), len(config.Schema()))
	}
	for _, key := range view.Keys {
		if key.Source != string(config.SourceDefault) {
			t.Errorf("%s source = %q, want default", key.Path, key.Source)
		}
	}
	capacity := keyByPath(t, view, "cache.capacity")
	if capacity.EffectiveValue != capacity.Default {
		t.Errorf("cache.capacity effective value = %q, want the default %q", capacity.EffectiveValue, capacity.Default)
	}
}

// TestViewReportsAnUnwritableConfigurationFile covers a read-only mount or a
// deployment whose process cannot replace the file: the page degrades to
// read-only instead of showing an edit it could never save.
func TestViewReportsAnUnwritableConfigurationFile(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		path := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
		if err := os.Chmod(path, 0o444); err != nil {
			t.Fatalf("chmod configuration file: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

		view := loadView(t, path)
		if view.ReadOnlyReason != configctl.ReadOnlyFileNotWritable {
			t.Fatalf("read-only reason = %q, want %q", view.ReadOnlyReason, configctl.ReadOnlyFileNotWritable)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		// The atomic write-back replaces the target path, so a symlink cannot
		// be written back through: the page treats it as read-only instead of
		// promising a save the writer would refuse.
		target := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
		link := filepath.Join(filepath.Dir(target), "config.toml")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink configuration file: %v", err)
		}

		view := loadView(t, link)
		if view.ReadOnlyReason != configctl.ReadOnlyFileNotWritable {
			t.Fatalf("read-only reason = %q, want %q", view.ReadOnlyReason, configctl.ReadOnlyFileNotWritable)
		}
	})

	t.Run("directory", func(t *testing.T) {
		path := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
		dir := filepath.Dir(path)
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatalf("chmod configuration directory: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		view := loadView(t, path)
		if view.ReadOnlyReason != configctl.ReadOnlyFileNotWritable {
			t.Fatalf("read-only reason = %q, want %q", view.ReadOnlyReason, configctl.ReadOnlyFileNotWritable)
		}
	})
}

// TestViewStatusStartsEmpty covers the placeholder of the save and reload
// status: the read-only walking skeleton reports a zero status until the save
// and reload tickets fill it in.
func TestViewStatusStartsEmpty(t *testing.T) {
	path := writeConfigFile(t, "[log]\nlevel = \"info\"\n")
	view := loadView(t, path)

	for name, status := range map[string]configctl.Status{"save": view.LastSave, "reload": view.LastReload} {
		if status.At != nil {
			t.Errorf("%s status has a timestamp, want the zero status", name)
		}
		if status.OK {
			t.Errorf("%s status reports OK=true, want the zero status", name)
		}
		if status.Error != "" {
			t.Errorf("%s status has error %q, want empty", name, status.Error)
		}
		if status.Keys == nil {
			t.Errorf("%s status keys are nil, want an empty list", name)
		}
		if len(status.Keys) != 0 {
			t.Errorf("%s status keys = %v, want empty", name, status.Keys)
		}
	}
}

// TestViewReportsTheConfigurationFileBaseline covers the conflict contract of
// the save path: the view carries a digest of the file bytes the page edited,
// so a save can refuse to overwrite a file another writer changed.
func TestViewReportsTheConfigurationFileBaseline(t *testing.T) {
	body := "backend = \"none\"\n\n[log]\nlevel = \"info\"\n"
	path := writeConfigFile(t, body)
	view := loadView(t, path)

	sum := sha256.Sum256([]byte(body))
	if want := hex.EncodeToString(sum[:]); view.Baseline != want {
		t.Errorf("baseline = %q, want the SHA-256 of the file %q", view.Baseline, want)
	}

	withoutFile, err := configctl.New("", config.Default(), config.Sources{}).View()
	if err != nil {
		t.Fatalf("configuration view: %v", err)
	}
	if withoutFile.Baseline != "" {
		t.Errorf("baseline without a configuration file = %q, want empty", withoutFile.Baseline)
	}
}
