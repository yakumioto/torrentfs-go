package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadFilesystemTest(t *testing.T, text string, env map[string]string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return load(path, lookupEnvironment(env))
}

func TestFilesystemSelectionAndUnusedBackendSettings(t *testing.T) {
	for _, backend := range []Backend{BackendNone, BackendFUSE, BackendSMB} {
		t.Run(string(backend), func(t *testing.T) {
			text := "backend = \"" + string(backend) + "\"\n[filesystem]\nread_timeout = \"2s\"\n" +
				"[fuse]\nmountpoint = \"/not-required-in-smb\"\n"
			env := map[string]string{}
			if backend == BackendSMB {
				env["TORRENTFS_USERNAME"] = "a user"
				env["TORRENTFS_PASSWORD"] = "test-only-password"
			}
			cfg, err := loadFilesystemTest(t, text, env)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Backend != backend || cfg.Filesystem.ReadTimeout != Duration(2*time.Second) {
				t.Fatalf("selection = %+v %+v", cfg.Backend, cfg.Filesystem)
			}
			if backend != BackendFUSE && cfg.FUSE.Mountpoint == "" {
				t.Fatal("unselected backend settings were discarded")
			}
		})
	}
}

func TestFilesystemFUSEMountpointDoesNotSelectBackend(t *testing.T) {
	cfg, err := loadFilesystemTest(t, "[fuse]\nmountpoint = \"/virtual-mount\"\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backend != BackendNone {
		t.Fatalf("backend = %q, want the default %q", cfg.Backend, BackendNone)
	}
}

func TestFilesystemFUSEWithoutMountpointIsRejected(t *testing.T) {
	_, err := loadFilesystemTest(t, "backend=\"fuse\"", nil)
	if err == nil {
		t.Fatal("FUSE without a mountpoint accepted")
	}
	if !strings.Contains(err.Error(), "fuse.mountpoint") {
		t.Fatalf("load error = %q, want fuse.mountpoint", err)
	}
}

func TestFilesystemRejectsInvalidValues(t *testing.T) {
	for _, text := range []string{
		"backend=\"unsupported\"",
		"[filesystem]\nread_timeout=\"0s\"",
	} {
		if _, err := loadFilesystemTest(t, text, nil); err == nil {
			t.Fatalf("accepted invalid configuration %q", text)
		}
	}
}

// TestUnifiedCredentialsServeTheSMBOutput covers the shared credentials: the
// SMB output uses the top-level user name and password.
func TestUnifiedCredentialsServeTheSMBOutput(t *testing.T) {
	text := "backend=\"smb\"\nusername=\"not-a-Unix-account@example\"\npassword=\"  secret-with-spaces  \"\n" +
		"[http]\nlisten_addr=\"\"\n"
	cfg, err := loadFilesystemTest(t, text, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Username != "not-a-Unix-account@example" {
		t.Fatalf("username = %q, want the configured user name", cfg.Username)
	}
	if cfg.Password != "  secret-with-spaces  " {
		t.Fatal("password whitespace was changed")
	}
	cfg, err = loadFilesystemTest(t, text, map[string]string{
		"TORRENTFS_USERNAME": "overridden", "TORRENTFS_PASSWORD": strings.Repeat("p", 128),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Username != "overridden" || len(cfg.Password) != 128 {
		t.Fatal("the environment did not override the credentials")
	}
}

// TestInvalidCredentialsAreNotDisclosed covers the secrecy requirement: no
// error message names the password value.
func TestInvalidCredentialsAreNotDisclosed(t *testing.T) {
	const secret = "must-not-appear-in-errors"
	for _, text := range []string{
		"backend=\"smb\"\nusername=\"user\"\npassword=\"" + secret + "\n",
		"backend=\"none\"\npassword=\"" + secret + strings.Repeat("x", 80) + "\"\n[http.auth]\nenabled=true\n",
	} {
		_, err := loadFilesystemTest(t, text, nil)
		if err == nil {
			t.Fatal("invalid secret configuration accepted")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatal("secret appeared in error")
		}
	}
	for _, env := range []map[string]string{
		{"TORRENTFS_BACKEND": "smb", "TORRENTFS_USERNAME": "user"},
		{"TORRENTFS_BACKEND": "smb", "TORRENTFS_USERNAME": "user", "TORRENTFS_PASSWORD": ""},
		{"TORRENTFS_BACKEND": "smb", "TORRENTFS_USERNAME": "user", "TORRENTFS_PASSWORD": secret + "\n"},
	} {
		_, err := loadFilesystemTest(t, "", env)
		if err == nil {
			t.Fatal("invalid environment credentials accepted")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatal("environment password disclosed")
		}
	}
}

// TestSMBListenAddrExplicitEmptyIsRejected covers the decision that an
// explicitly empty listen address is a configuration mistake rather than a
// request for the default address: silently restoring the default hides a
// typo that would otherwise expose the SMB port on every interface.
func TestSMBListenAddrExplicitEmptyIsRejected(t *testing.T) {
	_, err := loadFilesystemTest(t, "backend=\"smb\"\n[smb]\nlisten_addr=\"\"\n", map[string]string{
		"TORRENTFS_USERNAME": "user", "TORRENTFS_PASSWORD": "test-only-password",
	})
	if err == nil {
		t.Fatal("an explicitly empty smb.listen_addr was accepted")
	}
	if !strings.Contains(err.Error(), "smb.listen_addr") {
		t.Fatalf("load error = %q, want smb.listen_addr", err)
	}
}

// TestSMBListenAddrOmittedKeepsDefault checks that an omitted listen address
// still yields the built-in default, so the single default source stays
// authoritative.
func TestSMBListenAddrOmittedKeepsDefault(t *testing.T) {
	cfg, err := loadFilesystemTest(t, "backend=\"smb\"\n", map[string]string{
		"TORRENTFS_USERNAME": "user", "TORRENTFS_PASSWORD": "test-only-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMB.ListenAddr != "0.0.0.0:1445" {
		t.Fatalf("smb.listen_addr = %q, want the built-in default", cfg.SMB.ListenAddr)
	}
}
