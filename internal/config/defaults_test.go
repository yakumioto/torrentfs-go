package config_test

import (
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// TestLoadDistinguishesExplicitZeroValuesFromOmittedKeys covers the contract
// that a configuration file is decoded onto the built-in defaults: a key the
// document sets wins even when its value is the zero value of its type, and a
// key the document omits keeps the default.
func TestLoadDistinguishesExplicitZeroValuesFromOmittedKeys(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantListen  string
		wantMount   string
		wantTimeout config.Duration
	}{
		{
			name:        "omitted keys keep the defaults",
			body:        "[log]\nlevel = \"debug\"\n",
			wantListen:  "127.0.0.1:8080",
			wantMount:   "",
			wantTimeout: config.Duration(30 * 1e9),
		},
		{
			name:        "an explicit empty listen address overrides the default",
			body:        "[http]\nlisten_addr = \"\"\n",
			wantListen:  "",
			wantMount:   "",
			wantTimeout: config.Duration(30 * 1e9),
		},
		{
			name:        "an explicit empty mountpoint overrides the default",
			body:        "[fuse]\nmountpoint = \"\"\n",
			wantListen:  "127.0.0.1:8080",
			wantMount:   "",
			wantTimeout: config.Duration(30 * 1e9),
		},
		{
			name:        "an explicit read timeout overrides the default",
			body:        "[filesystem]\nread_timeout = \"250ms\"\n",
			wantListen:  "127.0.0.1:8080",
			wantMount:   "",
			wantTimeout: config.Duration(250 * 1e6),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := config.Load(writeConfig(t, tt.body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.HTTP.ListenAddr != tt.wantListen {
				t.Fatalf("http.listen_addr = %q, want %q", cfg.HTTP.ListenAddr, tt.wantListen)
			}
			if cfg.FUSE.Mountpoint != tt.wantMount {
				t.Fatalf("fuse.mountpoint = %q, want %q", cfg.FUSE.Mountpoint, tt.wantMount)
			}
			if cfg.Filesystem.ReadTimeout != tt.wantTimeout {
				t.Fatalf("filesystem.read_timeout = %s, want %s", cfg.Filesystem.ReadTimeout, tt.wantTimeout)
			}
		})
	}
}

// TestLoadWithAndWithoutFileAgreeOnDefaults covers the single-source
// requirement: loading a configuration file that omits a key yields the same
// default as loading no file at all.
func TestLoadWithAndWithoutFileAgreeOnDefaults(t *testing.T) {
	withFile, err := config.Load(writeConfig(t, "[log]\nlevel = \"debug\"\n"))
	if err != nil {
		t.Fatalf("Load with file: %v", err)
	}
	withoutFile, err := config.Load(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("Load without file: %v", err)
	}
	defaults := config.Default()
	if withFile.Cache != defaults.Cache {
		t.Fatalf("cache = %+v, want defaults %+v", withFile.Cache, defaults.Cache)
	}
	if withFile.SMB != defaults.SMB {
		t.Fatalf("smb = %+v, want defaults %+v", withFile.SMB, defaults.SMB)
	}
	if withFile.Filesystem != defaults.Filesystem {
		t.Fatalf("filesystem = %+v, want defaults %+v", withFile.Filesystem, defaults.Filesystem)
	}
	if withFile.FUSE != defaults.FUSE {
		t.Fatalf("fuse = %+v, want defaults %+v", withFile.FUSE, defaults.FUSE)
	}
	if withFile.HTTP.ListenAddr != withoutFile.HTTP.ListenAddr {
		t.Fatalf("http.listen_addr = %q with a file and %q without one",
			withFile.HTTP.ListenAddr, withoutFile.HTTP.ListenAddr)
	}
	if withFile.Filesystem.ReadTimeout != withoutFile.Filesystem.ReadTimeout {
		t.Fatalf("filesystem.read_timeout = %s with a file and %s without one",
			withFile.Filesystem.ReadTimeout, withoutFile.Filesystem.ReadTimeout)
	}
}

// TestLoadRejectsRemovedMountSection covers the removed "[mount]" section: it
// must be rejected with the configuration keys that replace it.
func TestLoadRejectsRemovedMountSection(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want []string
	}{
		{
			name: "read timeout",
			body: "[mount]\nread_timeout = \"3s\"\n",
			want: []string{"mount.read_timeout", "filesystem.read_timeout"},
		},
		{
			name: "allow other",
			body: "[mount]\nallow_other = true\n",
			want: []string{"mount.allow_other", "fuse.allow_other"},
		},
		{
			name: "the section itself",
			body: "[mount]\nunused = true\n",
			want: []string{"mount"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(writeConfig(t, tt.body))
			if err == nil {
				t.Fatal("Load accepted the removed [mount] section")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Load error = %q, want it to name %q", err, want)
				}
			}
		})
	}
}
