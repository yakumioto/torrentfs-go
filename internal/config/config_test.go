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
	if cfg.HTTP.ListenAddr != "127.0.0.1:8080" {
		t.Fatalf("Default listen address = %q, want 127.0.0.1:8080", cfg.HTTP.ListenAddr)
	}
	if cfg.HTTP.MaxUploadBytes != 10<<20 {
		t.Fatalf("Default max upload = %d, want %d", cfg.HTTP.MaxUploadBytes, 10<<20)
	}
	if cfg.HTTP.BearerToken != "" {
		t.Fatalf("Default bearer token = %q, want empty", cfg.HTTP.BearerToken)
	}
	wantIdentity := config.Identity{
		TrackerUserAgent:               "qBittorrent/4.4.0",
		PeerIDPrefix:                   "-qB4400-",
		ExtendedHandshakeClientVersion: "qBittorrent/4.4.0",
	}
	if cfg.Identity != wantIdentity {
		t.Fatalf("Default identity = %+v, want %+v", cfg.Identity, wantIdentity)
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

[identity]
tracker_user_agent = "torrentfs-test/1.0"
peer_id_prefix = "-TS1000-"
extended_handshake_client_version = "torrentfs-test/1.0"
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
	wantIdentity := config.Identity{
		TrackerUserAgent:               "torrentfs-test/1.0",
		PeerIDPrefix:                   "-TS1000-",
		ExtendedHandshakeClientVersion: "torrentfs-test/1.0",
	}
	if cfg.Identity != wantIdentity {
		t.Fatalf("Identity = %+v, want %+v", cfg.Identity, wantIdentity)
	}
}

func TestLoadHTTPSection(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	payloadDir := filepath.Join(t.TempDir(), "payload")
	path := writeConfig(t, `
[paths]
data_dir = `+quote(dataDir)+`
payload_dir = `+quote(payloadDir)+`

[http]
listen_addr = "127.0.0.1:9000"
bearer_token = "secret-token"
max_upload_bytes = 2048
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Paths.PayloadDir != payloadDir {
		t.Fatalf("PayloadDir = %q, want %q", cfg.Paths.PayloadDir, payloadDir)
	}
	if cfg.HTTP.ListenAddr != "127.0.0.1:9000" {
		t.Fatalf("ListenAddr = %q", cfg.HTTP.ListenAddr)
	}
	if cfg.HTTP.BearerToken != "secret-token" {
		t.Fatalf("BearerToken = %q", cfg.HTTP.BearerToken)
	}
	if cfg.HTTP.MaxUploadBytes != 2048 {
		t.Fatalf("MaxUploadBytes = %d, want 2048", cfg.HTTP.MaxUploadBytes)
	}
}

func TestLoadClientIdentityFormats(t *testing.T) {
	tests := []struct {
		name           string
		trackerAgent   string
		peerIDPrefix   string
		handshakeValue string
	}{
		{
			name:           "qBittorrent 4.4.0",
			trackerAgent:   "qBittorrent/4.4.0",
			peerIDPrefix:   "-qB4400-",
			handshakeValue: "qBittorrent/4.4.0",
		},
		{
			name:           "Transmission 3.00",
			trackerAgent:   "Transmission/3.00",
			peerIDPrefix:   "-TR3000-",
			handshakeValue: "Transmission/3.00",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, "[paths]\ndata_dir = "+quote(filepath.Join(t.TempDir(), "data"))+"\n\n[identity]\n"+
				"tracker_user_agent = "+quote(tt.trackerAgent)+"\n"+
				"peer_id_prefix = "+quote(tt.peerIDPrefix)+"\n"+
				"extended_handshake_client_version = "+quote(tt.handshakeValue)+"\n")
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Identity.TrackerUserAgent != tt.trackerAgent {
				t.Fatalf("TrackerUserAgent = %q, want %q", cfg.Identity.TrackerUserAgent, tt.trackerAgent)
			}
			if cfg.Identity.PeerIDPrefix != tt.peerIDPrefix {
				t.Fatalf("PeerIDPrefix = %q, want %q", cfg.Identity.PeerIDPrefix, tt.peerIDPrefix)
			}
			if cfg.Identity.ExtendedHandshakeClientVersion != tt.handshakeValue {
				t.Fatalf("ExtendedHandshakeClientVersion = %q, want %q", cfg.Identity.ExtendedHandshakeClientVersion, tt.handshakeValue)
			}
		})
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
	if cfg.Identity != defaults.Identity {
		t.Fatalf("Identity = %+v, want defaults %+v", cfg.Identity, defaults.Identity)
	}
}

func TestLoadUsesDefaultsForEmptyIdentitySection(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	path := writeConfig(t, "[paths]\ndata_dir = "+quote(dataDir)+"\n\n[identity]\n")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wantIdentity := config.Default().Identity
	if cfg.Identity != wantIdentity {
		t.Fatalf("Identity = %+v, want defaults %+v", cfg.Identity, wantIdentity)
	}
}

func TestLoadAllowsExplicitEmptyIdentityValues(t *testing.T) {
	path := writeConfig(t, "[identity]\ntracker_user_agent = \"\"\npeer_id_prefix = \"\"\nextended_handshake_client_version = \"\"\n")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Identity != (config.Identity{}) {
		t.Fatalf("Identity = %+v, want explicit empty values", cfg.Identity)
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
		{
			name: "invalid listen address",
			setup: func(cfg *config.Config) {
				cfg.HTTP.ListenAddr = "127.0.0.1"
			},
			field: "http.listen_addr",
		},
		{
			name: "non-positive max upload",
			setup: func(cfg *config.Config) {
				cfg.HTTP.MaxUploadBytes = 0
			},
			field: "http.max_upload_bytes",
		},
		{
			name: "bearer token with newline",
			setup: func(cfg *config.Config) {
				cfg.HTTP.BearerToken = "token\n"
			},
			field: "http.bearer_token",
		},
		{
			name: "non-loopback address without token",
			setup: func(cfg *config.Config) {
				cfg.HTTP.ListenAddr = "0.0.0.0:8080"
			},
			field: "http.listen_addr",
		},
		{
			name: "empty host without token",
			setup: func(cfg *config.Config) {
				cfg.HTTP.ListenAddr = ":8080"
			},
			field: "http.listen_addr",
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

func TestValidateRejectsInvalidIdentityValues(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*config.Config)
		field string
	}{
		{
			name: "peer ID prefix too long",
			setup: func(cfg *config.Config) {
				cfg.Identity.PeerIDPrefix = strings.Repeat("x", 21)
			},
			field: "identity.peer_id_prefix",
		},
		{
			name: "tracker user agent contains carriage return",
			setup: func(cfg *config.Config) {
				cfg.Identity.TrackerUserAgent = "torrentfs\r/1.0"
			},
			field: "identity.tracker_user_agent",
		},
		{
			name: "tracker user agent contains line feed",
			setup: func(cfg *config.Config) {
				cfg.Identity.TrackerUserAgent = "torrentfs\n/1.0"
			},
			field: "identity.tracker_user_agent",
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
		})
	}
}

func TestValidateAcceptsIdentityValues(t *testing.T) {
	cfg := config.Default()
	cfg.Identity = config.Identity{
		TrackerUserAgent:               "torrentfs test/1.0 (linux)",
		PeerIDPrefix:                   strings.Repeat("x", 20),
		ExtendedHandshakeClientVersion: "torrentfs test/1.0 (linux)",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	cfg.Identity.PeerIDPrefix = "客户端"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate non-ASCII prefix: %v", err)
	}
}

func TestValidateAcceptsExposedAddressWithToken(t *testing.T) {
	cfg := config.Default()
	cfg.HTTP.ListenAddr = "0.0.0.0:9000"
	cfg.HTTP.BearerToken = "secret"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate exposed address with token: %v", err)
	}

	cfg = config.Default()
	cfg.HTTP.ListenAddr = "" // HTTP disabled
	cfg.HTTP.BearerToken = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate disabled HTTP: %v", err)
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
