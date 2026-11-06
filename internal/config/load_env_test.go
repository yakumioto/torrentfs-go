package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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
		"TORRENTFS_FUSE_ALLOW_OTHER":                           "true",
		"TORRENTFS_FILESYSTEM_READ_TIMEOUT":                    "250ms",
		"TORRENTFS_PROXY_SOCKS5_URL":                           "socks5h://proxy.example:1080",
		"TORRENTFS_CACHE_CAPACITY":                             "123456B",
		"TORRENTFS_IDENTITY_TRACKER_USER_AGENT":                "torrentfs-test/1.0",
		"TORRENTFS_IDENTITY_PEER_ID_PREFIX":                    "-TS1000-",
		"TORRENTFS_IDENTITY_EXTENDED_HANDSHAKE_CLIENT_VERSION": "torrentfs-test/1.0",
		"TORRENTFS_HTTP_LISTEN_ADDR":                           "127.0.0.1:9090",
		"TORRENTFS_HTTP_MAX_UPLOAD_SIZE":                       "2048B",
		"TORRENTFS_HTTP_AUTH_ENABLED":                          "true",
		environmentUsername:                                    "alice",
		environmentPassword:                                    "password",
		"TORRENTFS_HTTP_AUTH_TOKEN_TTL":                        "45m",
		"TORRENTFS_STRM_BASE_URL":                              "https://media.example/torrentfs/",
		"TORRENTFS_LOG_LEVEL":                                  "debug",
		"TORRENTFS_LOG_FORMAT":                                 "json",
		"TORRENTFS_LOG_ADD_SOURCE":                             "true",
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
	if !got.FUSE.AllowOther {
		t.Fatal("fuse.allow_other = false, want true")
	}
	if got.Filesystem.ReadTimeout != Duration(250*time.Millisecond) {
		t.Fatalf("filesystem.read_timeout = %s, want 250ms", got.Filesystem.ReadTimeout)
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
	if got.STRM.BaseURL != "https://media.example/torrentfs/" {
		t.Fatalf("strm = %+v", got.STRM)
	}
	if got.Log != (Log{Level: "debug", Format: "json", AddSource: true}) {
		t.Fatalf("log = %+v", got.Log)
	}

	// Every environment variable the test sets must be one the schema
	// derives, so a new configuration key needs no second registration.
	wantNames := make(map[string]bool)
	for _, key := range Schema() {
		if key.Overridable {
			wantNames[key.Environment] = true
		}
	}
	for _, exception := range EnvironmentExceptions() {
		wantNames[exception.Name] = true
	}
	for name := range values {
		if !wantNames[name] {
			t.Fatalf("environment variable %q is not declared", name)
		}
	}
}

func TestLoadFileAndEnvironmentPrecedence(t *testing.T) {
	path := writeConfigFile(t, "[cache]\ncapacity = \"123B\"\n[http]\nmax_upload_size = \"1024B\"\n")

	got, err := load(path, lookupEnvironment(map[string]string{
		"TORRENTFS_CACHE_CAPACITY":       "456B",
		"TORRENTFS_HTTP_MAX_UPLOAD_SIZE": "2048B",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Cache.CapacityBytes != 456 {
		t.Fatalf("CapacityBytes = %d, want environment value 456", got.Cache.CapacityBytes)
	}
	if got.HTTP.MaxUploadBytes != 2048 {
		t.Fatalf("MaxUploadBytes = %d, want environment value 2048", got.HTTP.MaxUploadBytes)
	}

	defaults, err := load("", lookupEnvironment(nil))
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	if defaults.Cache.CapacityBytes != ByteQuantity(defaultCacheCapacityBytes) {
		t.Fatalf("default cache capacity = %d, want %d", defaults.Cache.CapacityBytes, defaultCacheCapacityBytes)
	}
}

func TestLoadEnvironmentReadTimeoutOverridesFileBeforeValidation(t *testing.T) {
	for _, fileValue := range []string{"30s", "0s"} {
		t.Run(fileValue, func(t *testing.T) {
			path := writeConfigFile(t, "[filesystem]\nread_timeout = \""+fileValue+"\"\n")
			cfg, err := load(path, lookupEnvironment(map[string]string{
				"TORRENTFS_FILESYSTEM_READ_TIMEOUT": "250ms",
			}))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.Filesystem.ReadTimeout != Duration(250*time.Millisecond) {
				t.Fatalf("filesystem.read_timeout = %s, want 250ms", cfg.Filesystem.ReadTimeout)
			}
		})
	}
}

func TestLoadEnvironmentRejectsInvalidReadTimeout(t *testing.T) {
	for _, raw := range []string{"", "30", "invalid", "0s", "-1s"} {
		t.Run(raw, func(t *testing.T) {
			_, err := load("", lookupEnvironment(map[string]string{
				"TORRENTFS_FILESYSTEM_READ_TIMEOUT": raw,
			}))
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("load error = %v, want ErrInvalid", err)
			}
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) || validationErr.Field != "filesystem.read_timeout" {
				t.Fatalf("load error = %v, want filesystem.read_timeout validation error", err)
			}
		})
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

func TestLoadIgnoresUnitSuffixedEnvironment(t *testing.T) {
	got, err := load("", lookupEnvironment(map[string]string{
		"TORRENTFS_CACHE_CAPACITY_BYTES":  "32MiB",
		"TORRENTFS_HTTP_MAX_UPLOAD_BYTES": "8GB",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(got, Default()) {
		t.Fatalf("config changed after removed environment variables: %+v", got)
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
		{name: "cache", env: "TORRENTFS_CACHE_CAPACITY", raw: "not-a-capacity", field: "cache.capacity"},
		{name: "upload", env: "TORRENTFS_HTTP_MAX_UPLOAD_SIZE", raw: "not-a-limit", field: "http.max_upload_size"},
		{name: "bool", env: "TORRENTFS_HTTP_AUTH_ENABLED", raw: "not-a-bool", field: "http.auth.enabled"},
		{name: "fuse bool", env: "TORRENTFS_FUSE_ALLOW_OTHER", raw: "not-a-bool", field: "fuse.allow_other"},
		{name: "duration", env: "TORRENTFS_HTTP_AUTH_TOKEN_TTL", raw: "not-a-duration", field: "http.auth.token_ttl"},
		{name: "read timeout", env: "TORRENTFS_FILESYSTEM_READ_TIMEOUT", raw: "not-a-duration", field: "filesystem.read_timeout"},
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

func TestLoadEnvironmentByteQuantitiesRequireUnits(t *testing.T) {
	tests := []struct {
		name string
		env  string
	}{
		{name: "cache", env: "TORRENTFS_CACHE_CAPACITY"},
		{name: "upload", env: "TORRENTFS_HTTP_MAX_UPLOAD_SIZE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load("", lookupEnvironment(map[string]string{tt.env: "32"}))
			if err == nil {
				t.Fatal("load succeeded")
			}
			if !strings.Contains(err.Error(), "byte unit") {
				t.Fatalf("load error = %q, want a byte-unit hint", err)
			}
			if strings.Contains(err.Error(), "32") {
				t.Fatalf("load error = %q, must not include raw environment value", err)
			}
		})
	}
}

func TestLoadEnvironmentAcceptsHumanReadableByteQuantities(t *testing.T) {
	got, err := load("", lookupEnvironment(map[string]string{
		"TORRENTFS_CACHE_CAPACITY":       "32MiB",
		"TORRENTFS_HTTP_MAX_UPLOAD_SIZE": "8GB",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Cache.CapacityBytes != 32<<20 {
		t.Fatalf("cache capacity = %d, want %d", got.Cache.CapacityBytes, 32<<20)
	}
	if got.HTTP.MaxUploadBytes != 8_000_000_000 {
		t.Fatalf("max upload = %d, want 8000000000", got.HTTP.MaxUploadBytes)
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
		environmentUsername: "env-user",
		environmentPassword: "password with spaces",
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
			environment: map[string]string{environmentUsername: "env-user"},
			wantError:   true,
		},
		{
			name: "password only does not borrow TOML username",
			config: `[http.auth]
enabled = true
username = "file-user"
password_hash = "file-hash"
`,
			environment: map[string]string{environmentPassword: "secret"},
			wantError:   true,
		},
		{
			name:   "empty username",
			config: "[http.auth]\nenabled = true\n",
			environment: map[string]string{
				environmentUsername: "",
				environmentPassword: "secret",
			},
			wantError: true,
		},
		{
			name:   "empty password",
			config: "[http.auth]\nenabled = true\n",
			environment: map[string]string{
				environmentUsername: "alice",
				environmentPassword: "",
			},
			wantError: true,
		},
		{
			name: "disabled ignores complete pair",
			environment: map[string]string{
				environmentUsername: "invalid\nusername",
				environmentPassword: strings.Repeat("x", 73),
			},
		},
		{
			name:        "disabled ignores half pair",
			environment: map[string]string{environmentUsername: "alice"},
		},
		{
			name: "environment enables auth",
			environment: map[string]string{
				"TORRENTFS_HTTP_AUTH_ENABLED": "true",
				environmentUsername:           "alice",
				environmentPassword:           "secret",
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
				environmentUsername:           "alice",
				environmentPassword:           tt.password,
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

// lookupEnvironment injects a fixed set of environment variables. It is the
// seam every configuration test drives the loader through.
func lookupEnvironment(values map[string]string) envLookup {
	return mapEnvironment(values)
}

// mapEnvironment adapts a map of environment variables to the envLookup seam.
// The map is the source of truth for both lookup and enumeration, so a test
// that adds a variable sees it in both.
type mapEnvironment map[string]string

// Lookup returns the value of one environment variable.
func (m mapEnvironment) Lookup(name string) (string, bool) {
	value, ok := m[name]
	return value, ok
}

// Names returns the names of every environment variable that is set.
func (m mapEnvironment) Names() []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "torrentfs.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
