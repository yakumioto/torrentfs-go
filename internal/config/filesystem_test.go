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
	for _, backend := range []string{BackendNone, BackendFUSE, BackendSMB} {
		t.Run(backend, func(t *testing.T) {
			text := "[filesystem]\nbackend = \"" + backend + "\"\nread_timeout = \"2s\"\n" +
				"[fuse]\nmountpoint = \"/not-required-in-smb\"\n[smb]\npassword_file = \"/not-required-in-fuse\"\n"
			env := map[string]string{}
			if backend == BackendSMB {
				env["TORRENTFS_USERNAME"] = "a user"
				env["TORRENTFS_PASSWORD"] = "test-only-password"
			}
			cfg, err := loadFilesystemTest(t, text, env)
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.ResolveFilesystem(); err != nil {
				t.Fatal(err)
			}
			if cfg.Filesystem.Backend != backend || cfg.Filesystem.ReadTimeout != Duration(2*time.Second) {
				t.Fatalf("selection = %+v", cfg.Filesystem)
			}
			if backend != BackendFUSE && cfg.FUSE.Mountpoint == "" {
				t.Fatal("unselected backend settings were discarded")
			}
		})
	}
}

func TestFilesystemFUSEMountpointSelectsBackend(t *testing.T) {
	cfg, err := loadFilesystemTest(t, "[fuse]\nmountpoint = \"/virtual-mount\"\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ResolveFilesystem(); err != nil {
		t.Fatal(err)
	}
	if cfg.Filesystem.Backend != BackendFUSE || cfg.FUSE.Mountpoint != "/virtual-mount" {
		t.Fatalf("fuse selection = %+v %+v", cfg.Filesystem, cfg.FUSE)
	}
}

func TestFilesystemFUSEWithoutMountpointIsRejected(t *testing.T) {
	cfg, err := loadFilesystemTest(t, "[filesystem]\nbackend=\"fuse\"", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ResolveFilesystem(); err == nil {
		t.Fatal("FUSE without a mountpoint accepted")
	}
}

func TestFilesystemRejectsInvalidValues(t *testing.T) {
	for _, text := range []string{
		"[filesystem]\nbackend=\"unsupported\"",
		"[filesystem]\nread_timeout=\"0s\"",
	} {
		if _, err := loadFilesystemTest(t, text, nil); err == nil {
			t.Fatalf("accepted invalid configuration %q", text)
		}
	}
}

func TestSMBSecretAndHTTPDisabled(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "smb-password")
	if err := os.WriteFile(secret, []byte("  secret-with-spaces  \r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	text := "[filesystem]\nbackend=\"smb\"\n[http]\nlisten_addr=\"\"\n" +
		"[http.auth]\nenabled=true\nusername=\"\"\ntoken_ttl=\"0s\"\n" +
		"[smb]\nusername=\"not-a-Unix-account@example\"\npassword_file=\"" + secret + "\""
	cfg, err := loadFilesystemTest(t, text, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMB.Password != "  secret-with-spaces  " {
		t.Fatal("password whitespace was changed")
	}
	if err := cfg.ResolveFilesystem(); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadFilesystemTest(t, text, map[string]string{
		"TORRENTFS_USERNAME": "overridden", "TORRENTFS_PASSWORD": strings.Repeat("p", 128),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMB.Username != "overridden" || len(cfg.SMB.Password) != 128 {
		t.Fatal("shared credential override was not applied")
	}
}

func TestSMBInvalidSecretsAreNotDisclosed(t *testing.T) {
	const secret = "must-not-appear-in-errors"
	for _, text := range []string{
		"[smb]\npassword=\"" + secret + "\"",
		"[filesystem]\nbackend=\"smb\"\n[smb]\nusername=\"user\"\npassword_file=\"/does-not-exist\"",
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
		{"TORRENTFS_FILESYSTEM_BACKEND": "smb", "TORRENTFS_USERNAME": "user"},
		{"TORRENTFS_FILESYSTEM_BACKEND": "smb", "TORRENTFS_USERNAME": "user", "TORRENTFS_PASSWORD": ""},
		{"TORRENTFS_FILESYSTEM_BACKEND": "smb", "TORRENTFS_USERNAME": "user", "TORRENTFS_PASSWORD": secret + "\n"},
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
	_, err := loadFilesystemTest(t, "[filesystem]\nbackend=\"smb\"\n[smb]\nlisten_addr=\"\"\n", map[string]string{
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
	cfg, err := loadFilesystemTest(t, "[filesystem]\nbackend=\"smb\"\n", map[string]string{
		"TORRENTFS_USERNAME": "user", "TORRENTFS_PASSWORD": "test-only-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMB.ListenAddr != "0.0.0.0:1445" {
		t.Fatalf("smb.listen_addr = %q, want the built-in default", cfg.SMB.ListenAddr)
	}
}
