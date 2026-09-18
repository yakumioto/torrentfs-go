package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadWithoutPathUsesDefaults(t *testing.T) {
	want := Default()
	got, err := load("", lookupEnvironment(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != want {
		t.Fatalf("config = %+v, want defaults %+v", got, want)
	}
}

func TestLoadEnvironmentBindings(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	values := map[string]string{
		"TORRENTFS_PATHS_DATA_DIR":                             dataDir,
		"TORRENTFS_CONNECTIONS_LISTEN_HOST":                    "127.0.0.1",
		"TORRENTFS_CONNECTIONS_LISTEN_PORT":                    "12345",
		"TORRENTFS_PROXY_SOCKS5_URL":                           "socks5h://proxy.example:1080",
		"TORRENTFS_CACHE_CAPACITY_BYTES":                       "123456",
		"TORRENTFS_IDENTITY_TRACKER_USER_AGENT":                "torrentfs-test/1.0",
		"TORRENTFS_IDENTITY_PEER_ID_PREFIX":                    "-TS1000-",
		"TORRENTFS_IDENTITY_EXTENDED_HANDSHAKE_CLIENT_VERSION": "torrentfs-test/1.0",
		"TORRENTFS_HTTP_LISTEN_ADDR":                           "127.0.0.1:9090",
		"TORRENTFS_HTTP_MAX_UPLOAD_BYTES":                      "2048",
		"TORRENTFS_HTTP_AUTH_ENABLED":                          "true",
		"TORRENTFS_HTTP_AUTH_USERNAME":                         "alice",
		"TORRENTFS_HTTP_AUTH_PASSWORD_HASH":                    "hash",
		"TORRENTFS_HTTP_AUTH_PASSWORD_HASH_FILE":               "",
		"TORRENTFS_HTTP_AUTH_TOKEN_TTL":                        "45m",
		"TORRENTFS_LOG_LEVEL":                                  "debug",
		"TORRENTFS_LOG_FORMAT":                                 "json",
		"TORRENTFS_LOG_ADD_SOURCE":                             "true",
		"TORRENTFS_FUSE_REQUIRED":                              "1",
	}
	got, err := load("", lookupEnvironment(values))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got.Paths.DataDir != dataDir {
		t.Fatalf("paths = %+v", got.Paths)
	}
	if got.Connections.ListenHost != "127.0.0.1" || got.Connections.ListenPort != 12345 {
		t.Fatalf("connections = %+v", got.Connections)
	}
	if got.Proxy.Socks5URL != "socks5h://proxy.example:1080" {
		t.Fatalf("proxy = %+v", got.Proxy)
	}
	if got.Cache.CapacityBytes != 123456 {
		t.Fatalf("cache = %+v", got.Cache)
	}
	if got.Identity != (Identity{
		TrackerUserAgent:               "torrentfs-test/1.0",
		PeerIDPrefix:                   "-TS1000-",
		ExtendedHandshakeClientVersion: "torrentfs-test/1.0",
	}) {
		t.Fatalf("identity = %+v", got.Identity)
	}
	if got.HTTP.ListenAddr != "127.0.0.1:9090" || got.HTTP.MaxUploadBytes != 2048 {
		t.Fatalf("http = %+v", got.HTTP)
	}
	if got.HTTP.Auth != (Auth{
		Enabled:      true,
		Username:     "alice",
		PasswordHash: "hash",
		TokenTTL:     Duration(45 * time.Minute),
	}) {
		t.Fatalf("auth = %+v", got.HTTP.Auth)
	}
	if got.Log != (Log{Level: "debug", Format: "json", AddSource: true}) {
		t.Fatalf("log = %+v", got.Log)
	}

	wantNames := make(map[string]bool, len(environmentBindings))
	for _, binding := range environmentBindings {
		wantNames[binding.name] = true
	}
	if len(environmentBindings) != 18 {
		t.Fatalf("environment binding count = %d, want 18", len(environmentBindings))
	}
	for name := range values {
		if name == "TORRENTFS_FUSE_REQUIRED" {
			continue
		}
		if !wantNames[name] {
			t.Fatalf("environment variable %q is not declared", name)
		}
	}
}

func TestLoadFileAndEnvironmentPrecedence(t *testing.T) {
	fileDataDir := filepath.Join(t.TempDir(), "file-data")
	path := writeConfigFile(t, "[paths]\ndata_dir = \""+fileDataDir+"\"\n\n[cache]\ncapacity_bytes = 123\n")

	got, err := load(path, lookupEnvironment(map[string]string{
		"TORRENTFS_PATHS_DATA_DIR":       filepath.Join(t.TempDir(), "env-data"),
		"TORRENTFS_CACHE_CAPACITY_BYTES": "456",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Paths.DataDir == fileDataDir {
		t.Fatalf("DataDir = %q, environment should override file", got.Paths.DataDir)
	}
	if got.Cache.CapacityBytes != 456 {
		t.Fatalf("CapacityBytes = %d, want environment value 456", got.Cache.CapacityBytes)
	}

	defaults, err := load("", lookupEnvironment(nil))
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	if defaults.Cache.CapacityBytes != defaultCacheCapacityBytes {
		t.Fatalf("default cache capacity = %d, want %d", defaults.Cache.CapacityBytes, defaultCacheCapacityBytes)
	}
}

func TestLoadEnvironmentCanReplaceInvalidFileValuesBeforeValidation(t *testing.T) {
	path := writeConfigFile(t, `[paths]
data_dir = ""

[connections]
listen_port = -1

[http]
listen_addr = "0.0.0.0:8080"
`)
	values := map[string]string{
		"TORRENTFS_PATHS_DATA_DIR":          filepath.Join(t.TempDir(), "env-data"),
		"TORRENTFS_CONNECTIONS_LISTEN_PORT": "1234",
		"TORRENTFS_HTTP_LISTEN_ADDR":        "127.0.0.1:8080",
	}
	got, err := load(path, lookupEnvironment(values))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Connections.ListenPort != 1234 || got.HTTP.ListenAddr != "127.0.0.1:8080" {
		t.Fatalf("config = %+v, environment values were not applied", got)
	}
}

func TestLoadEnvironmentAllowsExplicitEmptyStrings(t *testing.T) {
	path := writeConfigFile(t, `[proxy]
socks5_url = "socks5://proxy.example:1080"

[identity]
tracker_user_agent = "file-agent"
peer_id_prefix = "file-prefix"
extended_handshake_client_version = "file-version"
`)
	values := map[string]string{
		"TORRENTFS_PROXY_SOCKS5_URL":                           "",
		"TORRENTFS_IDENTITY_TRACKER_USER_AGENT":                "",
		"TORRENTFS_IDENTITY_PEER_ID_PREFIX":                    "",
		"TORRENTFS_IDENTITY_EXTENDED_HANDSHAKE_CLIENT_VERSION": "",
	}
	got, err := load(path, lookupEnvironment(values))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Proxy.Socks5URL != "" {
		t.Fatalf("optional string values were not cleared: proxy=%+v", got.Proxy)
	}
	if got.Identity != (Identity{}) {
		t.Fatalf("identity = %+v, want explicit empty values", got.Identity)
	}
}

func TestLoadEnvironmentParseErrorsIdentifyBindingWithoutRawValue(t *testing.T) {
	tests := []struct {
		name  string
		env   string
		raw   string
		field string
	}{
		{name: "port", env: "TORRENTFS_CONNECTIONS_LISTEN_PORT", raw: "not-a-port", field: "connections.listen_port"},
		{name: "cache", env: "TORRENTFS_CACHE_CAPACITY_BYTES", raw: "not-a-capacity", field: "cache.capacity_bytes"},
		{name: "upload", env: "TORRENTFS_HTTP_MAX_UPLOAD_BYTES", raw: "not-a-limit", field: "http.max_upload_bytes"},
		{name: "bool", env: "TORRENTFS_HTTP_AUTH_ENABLED", raw: "not-a-bool", field: "http.auth.enabled"},
		{name: "duration", env: "TORRENTFS_HTTP_AUTH_TOKEN_TTL", raw: "not-a-duration", field: "http.auth.token_ttl"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load("", lookupEnvironment(map[string]string{tt.env: tt.raw}))
			if err == nil {
				t.Fatal("load succeeded")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("load error = %v, want ErrInvalid", err)
			}
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("load error = %T %v, want ValidationError", err, err)
			}
			if validationErr.Field != tt.field {
				t.Fatalf("ValidationError.Field = %q, want %q", validationErr.Field, tt.field)
			}
			message := err.Error()
			if !strings.Contains(message, tt.env) || !strings.Contains(message, tt.field) {
				t.Fatalf("load error = %q, want environment and field names", message)
			}
			if strings.Contains(message, tt.raw) {
				t.Fatalf("load error = %q, must not include raw environment value", message)
			}
		})
	}
}

func TestLoadEnvironmentRejectsProxyCredentialsWithoutLeakingThem(t *testing.T) {
	for _, raw := range []string{
		"socks5://alice:secret@proxy.example/%zz",
		"socks5://alice:secret@proxy.example:bad",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := load("", lookupEnvironment(map[string]string{
				"TORRENTFS_PROXY_SOCKS5_URL": raw,
			}))
			if err == nil {
				t.Fatal("load succeeded")
			}
			if !strings.Contains(err.Error(), "proxy.socks5_url") {
				t.Fatalf("load error = %q, want proxy field", err)
			}
			for _, secret := range []string{"alice", "secret", raw} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("load error = %q, must not contain proxy credential or URL %q", err, secret)
				}
			}
		})
	}
}

func TestLoadEnvironmentAuthenticationSourceSwitch(t *testing.T) {
	path := writeConfigFile(t, `[http.auth]
enabled = true
username = "alice"
password_hash = "file-hash"
`)
	_, err := load(path, lookupEnvironment(map[string]string{
		"TORRENTFS_HTTP_AUTH_PASSWORD_HASH_FILE": "/run/secrets/hash",
	}))
	if err == nil {
		t.Fatal("load with two password sources succeeded")
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "http.auth.password_hash" {
		t.Fatalf("load error = %T %v, want password_hash validation error", err, err)
	}

	got, err := load(path, lookupEnvironment(map[string]string{
		"TORRENTFS_HTTP_AUTH_PASSWORD_HASH":      "",
		"TORRENTFS_HTTP_AUTH_PASSWORD_HASH_FILE": "/run/secrets/hash",
	}))
	if err != nil {
		t.Fatalf("load after clearing inline hash: %v", err)
	}
	if got.HTTP.Auth.PasswordHash != "" || got.HTTP.Auth.PasswordHashFile != "/run/secrets/hash" {
		t.Fatalf("auth sources = %+v", got.HTTP.Auth)
	}
}

func TestLoadWithDataDirOverridePrecedesValidation(t *testing.T) {
	path := writeConfigFile(t, "[paths]\ndata_dir = \"\"\n")
	dataDir := filepath.Join(t.TempDir(), "cli-data")

	got, err := loadWithDataDir(path, lookupEnvironment(map[string]string{
		"TORRENTFS_PATHS_DATA_DIR": "",
	}), dataDir, true)
	if err != nil {
		t.Fatalf("loadWithDataDir: %v", err)
	}
	if got.Paths.DataDir != dataDir {
		t.Fatalf("DataDir = %q, want CLI override %q", got.Paths.DataDir, dataDir)
	}
}

func lookupEnvironment(values map[string]string) envLookup {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "torrentfs.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
