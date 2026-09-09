package config_test

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

func TestDefault(t *testing.T) {
	cfg := config.Default()
	if cfg.Paths.DataDir == "" {
		t.Fatal("Default DataDir is empty")
	}
	if cfg.Cache.CapacityBytes != 64<<20 {
		t.Fatalf("Default cache capacity = %d, want %d", cfg.Cache.CapacityBytes, 64<<20)
	}
	if cfg.Proxy.Socks5URL != "" {
		t.Fatalf("Default proxy URL = %q, want empty", cfg.Proxy.Socks5URL)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate: %v", err)
	}
}

func TestLoadCompleteConfig(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	path := writeConfig(t, `
[paths]
data_dir = "`+dataDir+`"

[connections]
listen_host = "127.0.0.1"
listen_port = 23456

[proxy]
socks5_url = "socks5h://user:password@proxy.example:1080"

[cache]
capacity_bytes = 8192
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Paths.DataDir != dataDir {
		t.Fatalf("DataDir = %q, want %q", cfg.Paths.DataDir, dataDir)
	}
	if cfg.Connections.ListenHost != "127.0.0.1" || cfg.Connections.ListenPort != 23456 {
		t.Fatalf("Connections = %+v", cfg.Connections)
	}
	if cfg.Proxy.Socks5URL != "socks5h://user:password@proxy.example:1080" {
		t.Fatalf("Socks5URL = %q", cfg.Proxy.Socks5URL)
	}
	if cfg.Cache.CapacityBytes != 8192 {
		t.Fatalf("CapacityBytes = %d, want 8192", cfg.Cache.CapacityBytes)
	}
}

func TestLoadUsesDefaultsForOmittedValues(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	path := writeConfig(t, "[paths]\ndata_dir = "+quote(dataDir)+"\n")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defaults := config.Default()
	if cfg.Connections != defaults.Connections {
		t.Fatalf("Connections = %+v, want defaults %+v", cfg.Connections, defaults.Connections)
	}
	if cfg.Proxy != defaults.Proxy {
		t.Fatalf("Proxy = %+v, want defaults %+v", cfg.Proxy, defaults.Proxy)
	}
	if cfg.Cache != defaults.Cache {
		t.Fatalf("Cache = %+v, want defaults %+v", cfg.Cache, defaults.Cache)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := writeConfig(t, "[paths]\ndata_dir = \"/tmp/torrentfs\"\n\n[unknown]\nvalue = true\n")

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load unknown section succeeded")
	}
	var strictErr *toml.StrictMissingError
	if !errors.As(err, &strictErr) {
		t.Fatalf("Load unknown section error = %T %v, want *toml.StrictMissingError", err, err)
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("Load unknown section error = %q, want unknown key details", err)
	}
}

func TestLoadWrapsSyntaxAndTypeErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "syntax",
			body: "[paths\ndata_dir = \"/tmp/torrentfs\"\n",
		},
		{
			name: "type",
			body: "[paths]\ndata_dir = \"/tmp/torrentfs\"\n\n[connections]\nlisten_port = \"bad\"\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(writeConfig(t, tt.body))
			if err == nil {
				t.Fatal("Load succeeded")
			}
			var decodeErr *toml.DecodeError
			if !errors.As(err, &decodeErr) {
				t.Fatalf("Load error = %T %v, want wrapped *toml.DecodeError", err, err)
			}
		})
	}
}

func TestLoadWrapsOpenError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.toml")
	_, err := config.Load(path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load missing error = %v, want os.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("Load missing error = %q, want path %q", err, path)
	}
}

func TestValidateRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*config.Config)
		field string
	}{
		{
			name: "empty data dir",
			setup: func(cfg *config.Config) {
				cfg.Paths.DataDir = ""
			},
			field: "paths.data_dir",
		},
		{
			name: "negative port",
			setup: func(cfg *config.Config) {
				cfg.Connections.ListenPort = -1
			},
			field: "connections.listen_port",
		},
		{
			name: "port too large",
			setup: func(cfg *config.Config) {
				cfg.Connections.ListenPort = 65536
			},
			field: "connections.listen_port",
		},
		{
			name: "negative cache",
			setup: func(cfg *config.Config) {
				cfg.Cache.CapacityBytes = -1
			},
			field: "cache.capacity_bytes",
		},
		{
			name: "wrong proxy scheme",
			setup: func(cfg *config.Config) {
				cfg.Proxy.Socks5URL = "http://proxy.example:8080"
			},
			field: "proxy.socks5_url",
		},
		{
			name: "missing proxy host",
			setup: func(cfg *config.Config) {
				cfg.Proxy.Socks5URL = "socks5://"
			},
			field: "proxy.socks5_url",
		},
		{
			name: "invalid proxy port",
			setup: func(cfg *config.Config) {
				cfg.Proxy.Socks5URL = "socks5://proxy.example:65536"
			},
			field: "proxy.socks5_url",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			tt.setup(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate succeeded")
			}
			if !errors.Is(err, config.ErrInvalid) {
				t.Fatalf("Validate error = %v, want config.ErrInvalid", err)
			}
			var validationErr *config.ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("Validate error = %T %v, want *config.ValidationError", err, err)
			}
			if validationErr.Field != tt.field {
				t.Fatalf("ValidationError.Field = %q, want %q", validationErr.Field, tt.field)
			}
			if !strings.Contains(err.Error(), tt.field) {
				t.Fatalf("Validate error = %q, want field name", err)
			}
		})
	}
}

func TestValidatePreservesProxyParseError(t *testing.T) {
	cfg := config.Default()
	cfg.Proxy.Socks5URL = "socks5://proxy.example/%zz"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate succeeded")
	}
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("Validate error = %T %v, want *config.ValidationError", err, err)
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("Validate error = %T %v, want wrapped *url.Error", err, err)
	}
}

func TestValidateAcceptsProxyURLs(t *testing.T) {
	for _, raw := range []string{
		"socks5://proxy.example",
		"socks5h://user:password@proxy.example:1",
		"SOCKS5://[::1]:65535",
	} {
		cfg := config.Default()
		cfg.Proxy.Socks5URL = raw
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate(%q): %v", raw, err)
		}
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "torrentfs.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func quote(value string) string {
	return `"` + value + `"`
}
