package config_test

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

func TestDefault(t *testing.T) {
	cfg := config.Default()
	if cfg.Cache.CapacityBytes != 2<<30 {
		t.Fatalf("Default cache capacity = %d, want %d", cfg.Cache.CapacityBytes, 2<<30)
	}
	if cfg.Log != (config.Log{Level: "info", Format: "text"}) {
		t.Fatalf("Default log = %+v, want info/text", cfg.Log)
	}
	if cfg.Mount.AllowOther {
		t.Fatal("Default mount.allow_other = true, want false")
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
	if cfg.HTTP.Auth.Enabled {
		t.Fatal("Default authentication enabled, want disabled")
	}
	if time.Duration(cfg.HTTP.Auth.TokenTTL) != 30*time.Minute {
		t.Fatalf("Default token TTL = %s, want 30m", cfg.HTTP.Auth.TokenTTL)
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
	path := writeConfig(t, `
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

[log]
level = "debug"
format = "json"
add_source = true

[mount]
allow_other = true
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Connections.ListenHost != "127.0.0.1" || cfg.Connections.ListenPort != 23456 {
		t.Fatalf("Connections = %+v", cfg.Connections)
	}
	if cfg.Proxy.Socks5URL != "socks5h://user:password@proxy.example:1080" {
		t.Fatalf("Socks5URL = %q", cfg.Proxy.Socks5URL)
	}
	if cfg.Log != (config.Log{Level: "debug", Format: "json", AddSource: true}) {
		t.Fatalf("Log = %+v, want debug/json/source", cfg.Log)
	}
	if !cfg.Mount.AllowOther {
		t.Fatal("Mount.AllowOther = false, want true")
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
	path := writeConfig(t, `
[http]
listen_addr = "127.0.0.1:9000"
max_upload_bytes = 2048

[http.auth]
enabled = true
username = "test-user"
password_hash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
token_ttl = "45m"
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.ListenAddr != "127.0.0.1:9000" {
		t.Fatalf("ListenAddr = %q", cfg.HTTP.ListenAddr)
	}
	if !cfg.HTTP.Auth.Enabled || cfg.HTTP.Auth.Username != "test-user" {
		t.Fatalf("Auth = %+v, want enabled test-user", cfg.HTTP.Auth)
	}
	if cfg.HTTP.Auth.PasswordHash == "" || cfg.HTTP.Auth.PasswordHashFile != "" {
		t.Fatalf("Auth password hash source = %+v, want inline hash", cfg.HTTP.Auth)
	}
	if time.Duration(cfg.HTTP.Auth.TokenTTL) != 45*time.Minute {
		t.Fatalf("Auth token TTL = %s, want 45m", cfg.HTTP.Auth.TokenTTL)
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
			path := writeConfig(t, "[identity]\n"+
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
	path := writeConfig(t, "")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defaults := config.Default()
	if !reflect.DeepEqual(cfg.Connections, defaults.Connections) {
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
	if cfg.HTTP.Auth != defaults.HTTP.Auth {
		t.Fatalf("Auth = %+v, want defaults %+v", cfg.HTTP.Auth, defaults.HTTP.Auth)
	}
	if cfg.Log != defaults.Log {
		t.Fatalf("Log = %+v, want defaults %+v", cfg.Log, defaults.Log)
	}
	if cfg.Mount != defaults.Mount {
		t.Fatalf("Mount = %+v, want defaults %+v", cfg.Mount, defaults.Mount)
	}
}

func TestLoadUsesDefaultsForEmptyIdentitySection(t *testing.T) {
	path := writeConfig(t, "[identity]\n")

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
	path := writeConfig(t, "[unknown]\nvalue = true\n")

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

func TestLoadRejectsRemovedPathsSection(t *testing.T) {
	_, err := config.Load(writeConfig(t, `[paths]
data_dir = "/tmp/torrentfs"
`))
	if err == nil {
		t.Fatal("Load with removed paths section succeeded")
	}
	var strictErr *toml.StrictMissingError
	if !errors.As(err, &strictErr) {
		t.Fatalf("Load error = %T %v, want *toml.StrictMissingError", err, err)
	}
	if !strings.Contains(err.Error(), "paths") {
		t.Fatalf("Load error = %q, want paths key details", err)
	}
}

func TestLoadWrapsSyntaxAndTypeErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "syntax",
			body: "[connections\nlisten_port = 123\n",
		},
		{
			name: "type",
			body: "[connections]\nlisten_port = \"bad\"\n",
		},
		{
			name: "duration",
			body: "[http.auth]\ntoken_ttl = \"not-a-duration\"\n",
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

func TestLoadRejectsRemovedBearerToken(t *testing.T) {
	_, err := config.Load(writeConfig(t, "[http]\nbearer_token = \"legacy\"\n"))
	if err == nil {
		t.Fatal("Load with removed bearer_token succeeded")
	}
	var strictErr *toml.StrictMissingError
	if !errors.As(err, &strictErr) {
		t.Fatalf("Load error = %T %v, want *toml.StrictMissingError", err, err)
	}
}

func TestValidateRejectsInvalidAuth(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*config.Config)
		field string
	}{
		{
			name: "enabled without username",
			setup: func(cfg *config.Config) {
				cfg.HTTP.Auth.Enabled = true
				cfg.HTTP.Auth.PasswordHash = "hash"
			},
			field: "http.auth.username",
		},
		{
			name: "enabled without password hash",
			setup: func(cfg *config.Config) {
				cfg.HTTP.Auth.Enabled = true
				cfg.HTTP.Auth.Username = "alice"
			},
			field: "http.auth.password_hash",
		},
		{
			name: "both password hash sources",
			setup: func(cfg *config.Config) {
				cfg.HTTP.Auth = config.Auth{
					Enabled:          true,
					Username:         "alice",
					PasswordHash:     "hash",
					PasswordHashFile: "hash-file",
					TokenTTL:         config.Duration(30 * time.Minute),
				}
			},
			field: "http.auth.password_hash",
		},
		{
			name: "disabled with credentials",
			setup: func(cfg *config.Config) {
				cfg.HTTP.Auth.Username = "alice"
			},
			field: "http.auth.username",
		},
		{
			name: "username control character",
			setup: func(cfg *config.Config) {
				cfg.HTTP.Auth = config.Auth{
					Enabled:      true,
					Username:     "alice\n",
					PasswordHash: "hash",
					TokenTTL:     config.Duration(time.Minute),
				}
			},
			field: "http.auth.username",
		},
		{
			name: "non-positive token TTL",
			setup: func(cfg *config.Config) {
				cfg.HTTP.Auth.TokenTTL = 0
			},
			field: "http.auth.token_ttl",
		},
		{
			name: "token TTL too long",
			setup: func(cfg *config.Config) {
				cfg.HTTP.Auth.TokenTTL = config.Duration(24*time.Hour + time.Nanosecond)
			},
			field: "http.auth.token_ttl",
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

func TestValidateRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*config.Config)
		field string
	}{
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
			name: "zero cache",
			setup: func(cfg *config.Config) {
				cfg.Cache.CapacityBytes = 0
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
		{
			name: "invalid log level",
			setup: func(cfg *config.Config) {
				cfg.Log.Level = "verbose"
			},
			field: "log.level",
		},
		{
			name: "invalid log level offset",
			setup: func(cfg *config.Config) {
				cfg.Log.Level = "info+2"
			},
			field: "log.level",
		},
		{
			name: "invalid log format",
			setup: func(cfg *config.Config) {
				cfg.Log.Format = "console"
			},
			field: "log.format",
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

func TestValidateAcceptsExposedAddressWithAuth(t *testing.T) {
	cfg := config.Default()
	cfg.HTTP.ListenAddr = "0.0.0.0:9000"
	cfg.HTTP.Auth = config.Auth{
		Enabled:      true,
		Username:     "test-user",
		PasswordHash: "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy",
		TokenTTL:     config.Duration(30 * time.Minute),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate exposed address with auth: %v", err)
	}

	cfg = config.Default()
	cfg.HTTP.ListenAddr = "" // HTTP disabled
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate disabled HTTP: %v", err)
	}
}

func TestValidatePreservesSanitizedProxyParseError(t *testing.T) {
	cfg := config.Default()
	cfg.Proxy.Socks5URL = "socks5://alice:secret@proxy.example/%zz"

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
	for _, secret := range []string{"alice", "secret", "socks5://alice:secret@proxy.example/%zz"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("Validate error = %q, must not contain proxy credential or URL %q", err, secret)
		}
	}
}

func TestValidateRejectsProxyCredentialsWithInvalidPort(t *testing.T) {
	cfg := config.Default()
	cfg.Proxy.Socks5URL = "socks5://alice:secret@proxy.example:bad"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate succeeded")
	}
	for _, secret := range []string{"alice", "secret", "socks5://alice:secret@proxy.example:bad"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("Validate error = %q, must not contain proxy credential or URL %q", err, secret)
		}
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
