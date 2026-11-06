package config

import (
	"errors"
	"strings"
	"testing"
)

// TestBackendStaysNoneWithoutAnExplicitSelection covers the removed
// heuristic: a configured mountpoint no longer selects the FUSE output, so an
// unset backend keeps its default.
func TestBackendStaysNoneWithoutAnExplicitSelection(t *testing.T) {
	cfg, err := load(writeConfigFile(t, "[fuse]\nmountpoint = \"/virtual-mount\"\nstrm_mountpoint = \"/virtual-strm\"\n"),
		lookupEnvironment(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Backend != BackendNone {
		t.Fatalf("backend = %q, want the default %q", cfg.Backend, BackendNone)
	}
}

// TestBackendSelectsTheOutputBackend covers the three output backends: each
// one is selected by the top-level key, from the configuration file and from
// the environment.
func TestBackendSelectsTheOutputBackend(t *testing.T) {
	tests := []struct {
		name string
		file string
		env  map[string]string
		want Backend
	}{
		{
			name: "none from the configuration file",
			file: "backend = \"none\"\n",
			want: BackendNone,
		},
		{
			name: "fuse from the configuration file",
			file: "backend = \"fuse\"\n[fuse]\nmountpoint = \"/virtual-mount\"\n",
			want: BackendFUSE,
		},
		{
			name: "smb from the environment",
			env: map[string]string{
				"TORRENTFS_BACKEND": "smb",
				environmentUsername: "smb-user",
				environmentPassword: "test-only-password",
			},
			want: BackendSMB,
		},
		{
			name: "the environment wins over the configuration file",
			file: "backend = \"fuse\"\n",
			env:  map[string]string{"TORRENTFS_BACKEND": "none"},
			want: BackendNone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := ""
			if tt.file != "" {
				path = writeConfigFile(t, tt.file)
			}
			cfg, err := load(path, lookupEnvironment(tt.env))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.Backend != tt.want {
				t.Fatalf("backend = %q, want %q", cfg.Backend, tt.want)
			}
		})
	}
}

// TestBackendRejectsAnUnknownValue covers the enumerated values of the output
// backend: the error names the key and the values it accepts.
func TestBackendRejectsAnUnknownValue(t *testing.T) {
	for _, source := range []string{"file", "environment"} {
		t.Run(source, func(t *testing.T) {
			path := ""
			var env map[string]string
			if source == "file" {
				path = writeConfigFile(t, "backend = \"unsupported\"\n")
			} else {
				env = map[string]string{"TORRENTFS_BACKEND": "unsupported"}
			}
			_, err := load(path, lookupEnvironment(env))
			if err == nil {
				t.Fatal("load accepted an unknown output backend")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("load error = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), "backend") {
				t.Fatalf("load error = %q, want the backend key", err)
			}
		})
	}
}

// TestEnumeratedValuesAreTheSameInEverySource covers the enumerated
// configuration keys: the configuration file and the environment reject the
// same values and name the same accepted ones, because the schema is the only
// place the values are written down.
func TestEnumeratedValuesAreTheSameInEverySource(t *testing.T) {
	for _, tt := range []struct {
		path   string
		file   string
		env    string
		values []string
	}{
		{path: "backend", file: "backend = \"unsupported\"\n", env: "TORRENTFS_BACKEND", values: []string{"none", "fuse", "smb"}},
		{path: "smb.strm", file: "[smb]\nstrm = \"unsupported\"\n", env: "TORRENTFS_SMB_STRM", values: []string{"auto", "on", "off"}},
	} {
		t.Run(tt.path, func(t *testing.T) {
			_, fileErr := load(writeConfigFile(t, tt.file), lookupEnvironment(nil))
			_, envErr := load("", lookupEnvironment(map[string]string{tt.env: "unsupported"}))
			if fileErr == nil || envErr == nil {
				t.Fatalf("load errors = (%v, %v), want both to reject the value", fileErr, envErr)
			}
			for _, want := range tt.values {
				if !strings.Contains(fileErr.Error(), want) {
					t.Fatalf("configuration file error = %q, want it to name %q", fileErr, want)
				}
				if !strings.Contains(envErr.Error(), want) {
					t.Fatalf("environment error = %q, want it to name %q", envErr, want)
				}
			}
			if !strings.Contains(fileErr.Error(), tt.path) {
				t.Fatalf("configuration file error = %q, want the key %q", fileErr, tt.path)
			}
		})
	}
}

// TestFUSEBackendRequiresAMountpoint covers the configuration completeness
// rule: the FUSE output needs a mountpoint, and the error names the key that
// provides one.
func TestFUSEBackendRequiresAMountpoint(t *testing.T) {
	_, err := load(writeConfigFile(t, "backend = \"fuse\"\n"), lookupEnvironment(nil))
	if err == nil {
		t.Fatal("load accepted the FUSE output without a mountpoint")
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "fuse.mountpoint" {
		t.Fatalf("load error = %v, want a fuse.mountpoint validation error", err)
	}
}

// TestFUSEBackendAcceptsTheSTRMMountpointAlone covers the second mountpoint of
// the FUSE output: the signed-streaming view is a mountpoint of its own.
func TestFUSEBackendAcceptsTheSTRMMountpointAlone(t *testing.T) {
	cfg, err := load(writeConfigFile(t, "backend = \"fuse\"\n[strm]\nbase_url = \"http://media.example:8080\"\n"+
		"[fuse]\nstrm_mountpoint = \"/virtual-strm\"\n"), lookupEnvironment(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Backend != BackendFUSE {
		t.Fatalf("backend = %q, want %q", cfg.Backend, BackendFUSE)
	}
}

// TestRemovedBackendNamesAreRejectedWithTheirReplacement covers the two
// removed names of the output backend: the old key and the old environment
// variable are refused, and the error names the new spelling.
func TestRemovedBackendNamesAreRejectedWithTheirReplacement(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		env     map[string]string
		removed string
		new     string
	}{
		{
			name:    "the old configuration key",
			path:    writeConfigFile(t, "[filesystem]\nbackend = \"fuse\"\n"),
			removed: "filesystem.backend",
			new:     "backend",
		},
		{
			name:    "the old environment variable",
			env:     map[string]string{"TORRENTFS_FILESYSTEM_BACKEND": "fuse"},
			removed: "TORRENTFS_FILESYSTEM_BACKEND",
			new:     "TORRENTFS_BACKEND",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(tt.path, lookupEnvironment(tt.env))
			if err == nil {
				t.Fatal("load accepted the removed name")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("load error = %v, want ErrInvalid", err)
			}
			for _, want := range []string{tt.removed, tt.new} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("load error = %q, want it to name %q", err, want)
				}
			}
		})
	}
}

// TestBackendKeepsTheSettingsOfTheUnselectedBackend covers the promise that
// switching the output backend needs no other change: the settings of the
// other backend stay in the effective configuration.
func TestBackendKeepsTheSettingsOfTheUnselectedBackend(t *testing.T) {
	cfg, err := load(writeConfigFile(t, "backend = \"none\"\n[fuse]\nmountpoint = \"/virtual-mount\"\n"+
		"[smb]\nlisten_addr = \"127.0.0.1:1445\"\n"), lookupEnvironment(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.FUSE.Mountpoint != "/virtual-mount" {
		t.Fatalf("fuse.mountpoint = %q, want the configured mountpoint", cfg.FUSE.Mountpoint)
	}
	if cfg.SMB.ListenAddr != "127.0.0.1:1445" {
		t.Fatalf("smb.listen_addr = %q, want the configured address", cfg.SMB.ListenAddr)
	}
}

// TestUnselectedBackendSettingsAreNotChecked covers the completeness rule of
// the output backends: the settings of a backend that is not selected do not
// have to be usable, so a deployment can keep both sets of settings and
// switch between them.
func TestUnselectedBackendSettingsAreNotChecked(t *testing.T) {
	cfg, err := load(writeConfigFile(t, "backend = \"none\"\n[smb]\nusername = \"\"\npassword_file = \"/does/not/exist\"\n"),
		lookupEnvironment(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Backend != BackendNone {
		t.Fatalf("backend = %q, want %q", cfg.Backend, BackendNone)
	}
}
