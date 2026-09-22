package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestLoadWithoutPathUsesDefaults(t *testing.T) {
	want := Default()
	got, err := load("", lookupEnvironment(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("config = %+v, want defaults %+v", got, want)
	}
}

func TestLoadEnvironmentBindings(t *testing.T) {
	values := map[string]string{
		"TORRENTFS_CONNECTIONS_LISTEN_HOST":                    "127.0.0.1",
		"TORRENTFS_CONNECTIONS_LISTEN_PORT":                    "12345",
		"TORRENTFS_CONNECTIONS_DISABLE_IPV4":                   "true",
		"TORRENTFS_CONNECTIONS_DISABLE_IPV6":                   "false",
		"TORRENTFS_CONNECTIONS_NO_PORT_FORWARDING":             "false",
		"TORRENTFS_CONNECTIONS_BOOTSTRAP_NODES":                "router.example:6881",
		"TORRENTFS_MOUNT_ALLOW_OTHER":                          "true",
		"TORRENTFS_PROXY_SOCKS5_URL":                           "socks5h://proxy.example:1080",
		"TORRENTFS_CACHE_CAPACITY_BYTES":                       "123456",
		"TORRENTFS_IDENTITY_TRACKER_USER_AGENT":                "torrentfs-test/1.0",
		"TORRENTFS_IDENTITY_PEER_ID_PREFIX":                    "-TS1000-",
		"TORRENTFS_IDENTITY_EXTENDED_HANDSHAKE_CLIENT_VERSION": "torrentfs-test/1.0",
		"TORRENTFS_HTTP_LISTEN_ADDR":                           "127.0.0.1:9090",
		"TORRENTFS_HTTP_MAX_UPLOAD_BYTES":                      "2048",
		"TORRENTFS_HTTP_AUTH_ENABLED":                          "true",
		httpUsernameEnvironment:                                "alice",
		httpPasswordEnvironment:                                "password",
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

	if got.Connections.ListenHost != "127.0.0.1" || got.Connections.ListenPort != 12345 {
		t.Fatalf("connections = %+v", got.Connections)
	}
	wantConnections := Connections{
		ListenHost:       "127.0.0.1",
		ListenPort:       12345,
		DisableIPv4:      true,
		NoPortForwarding: false,
		BootstrapNodes:   []string{"router.example:6881"},
	}
	if !reflect.DeepEqual(got.Connections, wantConnections) {
		t.Fatalf("connections = %+v, want %+v", got.Connections, wantConnections)
	}
	if !got.Mount.AllowOther {
		t.Fatal("mount.allow_other = false, want true")
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
	if !got.HTTP.Auth.Enabled || got.HTTP.Auth.Username != "alice" || got.HTTP.Auth.PasswordHashFile != "" || got.HTTP.Auth.PasswordHash == "password" {
		t.Fatalf("auth = %+v", got.HTTP.Auth)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(got.HTTP.Auth.PasswordHash), []byte("password")); err != nil {
		t.Fatalf("generated password hash does not match password: %v", err)
	}
	if cost, err := bcrypt.Cost([]byte(got.HTTP.Auth.PasswordHash)); err != nil || cost != bcrypt.DefaultCost {
		t.Fatalf("generated password hash cost = %d, error = %v, want %d", cost, err, bcrypt.DefaultCost)
	}
	if got.HTTP.Auth.TokenTTL != Duration(45*time.Minute) {
		t.Fatalf("auth token TTL = %s, want 45m", got.HTTP.Auth.TokenTTL)
	}
	if got.Log != (Log{Level: "debug", Format: "json", AddSource: true}) {
		t.Fatalf("log = %+v", got.Log)
	}

	wantNames := make(map[string]bool, len(environmentBindings))
	for _, binding := range environmentBindings {
		wantNames[binding.name] = true
	}
	if len(environmentBindings) != 19 {
		t.Fatalf("environment binding count = %d, want 19", len(environmentBindings))
	}
	if httpUsernameEnvironment != "TORRENTFS_USERNAME" || httpPasswordEnvironment != "TORRENTFS_PASSWORD" {
		t.Fatalf("special environment variables = %q, %q", httpUsernameEnvironment, httpPasswordEnvironment)
	}
	for name := range values {
		if name == "TORRENTFS_FUSE_REQUIRED" || name == httpUsernameEnvironment || name == httpPasswordEnvironment {
			continue
		}
		if !wantNames[name] {
			t.Fatalf("environment variable %q is not declared", name)
		}
	}
}

func TestLoadFileAndEnvironmentPrecedence(t *testing.T) {
	path := writeConfigFile(t, "[cache]\ncapacity_bytes = 123\n")

	got, err := load(path, lookupEnvironment(map[string]string{
		"TORRENTFS_CACHE_CAPACITY_BYTES": "456",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
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

func TestLoadIgnoresRemovedDataDirEnvironment(t *testing.T) {
	got, err := load("", lookupEnvironment(map[string]string{
		"TORRENTFS_PATHS_DATA_DIR": filepath.Join(t.TempDir(), "ignored"),
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(got, Default()) {
		t.Fatalf("config changed after removed environment variable: %+v", got)
	}
}

func TestLoadEnvironmentCanReplaceInvalidFileValuesBeforeValidation(t *testing.T) {
	path := writeConfigFile(t, `[connections]
listen_port = -1

[http]
listen_addr = "0.0.0.0:8080"
`)
	values := map[string]string{
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
		{name: "mount bool", env: "TORRENTFS_MOUNT_ALLOW_OTHER", raw: "not-a-bool", field: "mount.allow_other"},
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

func TestLoadEnvironmentAuthenticationPairOverridesTOMLSource(t *testing.T) {
	path := writeConfigFile(t, `[http.auth]
enabled = true
username = "file-user"
password_hash_file = "/run/secrets/hash"
`)
	got, err := load(path, lookupEnvironment(map[string]string{
		httpUsernameEnvironment: "env-user",
		httpPasswordEnvironment: "password with spaces",
	}))
	if err != nil {
		t.Fatalf("load with environment credentials: %v", err)
	}
	if got.HTTP.Auth.Username != "env-user" || got.HTTP.Auth.PasswordHashFile != "" {
		t.Fatalf("auth sources = %+v, want environment username and inline hash", got.HTTP.Auth)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(got.HTTP.Auth.PasswordHash), []byte("password with spaces")); err != nil {
		t.Fatalf("generated password hash does not match password: %v", err)
	}
}

func TestLoadEnvironmentAuthenticationPairRules(t *testing.T) {
	tests := []struct {
		name         string
		config       string
		environment  map[string]string
		wantError    bool
		wantUsername string
		wantPassword string
		wantHash     string
		wantHashFile string
	}{
		{
			name: "pair absent preserves inline source",
			config: `[http.auth]
enabled = true
username = "file-user"
password_hash = "file-hash"
`,
			wantUsername: "file-user",
			wantHash:     "file-hash",
		},
		{
			name: "pair absent preserves file source",
			config: `[http.auth]
enabled = true
username = "file-user"
password_hash_file = "/run/secrets/hash"
`,
			wantUsername: "file-user",
			wantHashFile: "/run/secrets/hash",
		},
		{
			name: "username only does not borrow TOML password",
			config: `[http.auth]
enabled = true
username = "file-user"
password_hash = "file-hash"
`,
			environment: map[string]string{httpUsernameEnvironment: "env-user"},
			wantError:   true,
		},
		{
			name: "password only does not borrow TOML username",
			config: `[http.auth]
enabled = true
username = "file-user"
password_hash = "file-hash"
`,
			environment: map[string]string{httpPasswordEnvironment: "secret"},
			wantError:   true,
		},
		{
			name:   "empty username",
			config: "[http.auth]\nenabled = true\n",
			environment: map[string]string{
				httpUsernameEnvironment: "",
				httpPasswordEnvironment: "secret",
			},
			wantError: true,
		},
		{
			name:   "empty password",
			config: "[http.auth]\nenabled = true\n",
			environment: map[string]string{
				httpUsernameEnvironment: "alice",
				httpPasswordEnvironment: "",
			},
			wantError: true,
		},
		{
			name: "disabled ignores complete pair",
			environment: map[string]string{
				httpUsernameEnvironment: "invalid\nusername",
				httpPasswordEnvironment: strings.Repeat("x", 73),
			},
		},
		{
			name:        "disabled ignores half pair",
			environment: map[string]string{httpUsernameEnvironment: "alice"},
		},
		{
			name: "environment enables auth",
			environment: map[string]string{
				"TORRENTFS_HTTP_AUTH_ENABLED": "true",
				httpUsernameEnvironment:       "alice",
				httpPasswordEnvironment:       "secret",
			},
			wantUsername: "alice",
			wantPassword: "secret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := load(writeConfigFile(t, tt.config), lookupEnvironment(tt.environment))
			if tt.wantError {
				if err == nil {
					t.Fatal("load succeeded")
				}
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("load error = %v, want ErrInvalid", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got.HTTP.Auth.Username != tt.wantUsername {
				t.Fatalf("auth username = %q, want %q", got.HTTP.Auth.Username, tt.wantUsername)
			}
			if tt.wantPassword == "" && got.HTTP.Auth.PasswordHash != tt.wantHash {
				t.Fatalf("auth password hash = %q, want %q", got.HTTP.Auth.PasswordHash, tt.wantHash)
			}
			if got.HTTP.Auth.PasswordHashFile != tt.wantHashFile {
				t.Fatalf("auth password hash file = %q, want %q", got.HTTP.Auth.PasswordHashFile, tt.wantHashFile)
			}
			if tt.wantPassword != "" {
				if err := bcrypt.CompareHashAndPassword([]byte(got.HTTP.Auth.PasswordHash), []byte(tt.wantPassword)); err != nil {
					t.Fatalf("generated password hash does not match password: %v", err)
				}
			}
		})
	}
}

func TestLoadEnvironmentAuthenticationPasswordLength(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{name: "72 ASCII bytes", password: strings.Repeat("x", 72)},
		{name: "72 UTF-8 bytes", password: strings.Repeat("密", 24)},
		{name: "73 ASCII bytes", password: strings.Repeat("x", 73), wantErr: true},
		{name: "75 UTF-8 bytes", password: strings.Repeat("密", 25), wantErr: true},
		{name: "preserves whitespace", password: "  secret  "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load("", lookupEnvironment(map[string]string{
				"TORRENTFS_HTTP_AUTH_ENABLED": "true",
				httpUsernameEnvironment:       "alice",
				httpPasswordEnvironment:       tt.password,
			}))
			if tt.wantErr {
				if err == nil {
					t.Fatal("load succeeded")
				}
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("load error = %v, want ErrInvalid", err)
				}
				if strings.Contains(err.Error(), tt.password) {
					t.Fatalf("load error = %q, must not contain password", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("load: %v", err)
			}
		})
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
