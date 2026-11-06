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
		"TORRENTFS_BACKEND":                                    "none",
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
		"TORRENTFS_USERNAME":                                   "alice",
		"TORRENTFS_PASSWORD":                                   "password",
		"TORRENTFS_HTTP_AUTH_TOKEN_TTL":                        "45m",
		"TORRENTFS_SMB_STRM":                                   "off",
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
	if got.Backend != BackendNone {
		t.Fatalf("backend = %q, want the environment value %q", got.Backend, BackendNone)
	}
	if got.SMB.STRM != STRMOff {
		t.Fatalf("smb.strm = %q, want the environment value %q", got.SMB.STRM, STRMOff)
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
	if got.Username != "alice" || got.Password != "password" {
		t.Fatalf("credentials = %q/%q, want the environment values", got.Username, got.Password)
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

// TestLoadRejectsUnknownEnvironmentNames covers the strictness of the
// configuration namespace: a variable that no configuration key derives is
// reported instead of ignored, so a misspelled name cannot start the process
// with a default value the user did not ask for.
func TestLoadRejectsUnknownEnvironmentNames(t *testing.T) {
	for _, name := range []string{
		"TORRENTFS_PATHS_DATA_DIR",
		"TORRENTFS_CACHE_CAPACITY_BYTES",
		"TORRENTFS_HTTP_MAX_UPLOAD_BYTES",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load("", lookupEnvironment(map[string]string{name: "value"}))
			if err == nil {
				t.Fatalf("load accepted the unknown environment variable %q", name)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("load error = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("load error = %q, want it to name %q", err, name)
			}
		})
	}
}

// TestLoadRejectsRemovedEnvironmentNames covers the removed environment
// variables of the output backend and of the STRM switch: the loader refuses
// them and names the variable that replaces them.
func TestLoadRejectsRemovedEnvironmentNames(t *testing.T) {
	tests := []struct {
		removed string
		new     string
	}{
		{removed: "TORRENTFS_FILESYSTEM_BACKEND", new: "TORRENTFS_BACKEND"},
		{removed: "TORRENTFS_SMB_STRM_ENABLED", new: "TORRENTFS_SMB_STRM"},
	}
	for _, tt := range tests {
		t.Run(tt.removed, func(t *testing.T) {
			_, err := load("", lookupEnvironment(map[string]string{tt.removed: "value"}))
			if err == nil {
				t.Fatal("load accepted the removed environment variable")
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

func TestLoadReportsEveryUnknownEnvironmentName(t *testing.T) {
	_, err := load("", lookupEnvironment(map[string]string{
		"TORRENTFS_CACHE_CAPACITY_BYTES":  "32MiB",
		"TORRENTFS_HTTP_MAX_UPLOAD_BYTES": "8GB",
	}))
	if err == nil {
		t.Fatal("load accepted unknown environment variables")
	}
	for _, name := range []string{"TORRENTFS_CACHE_CAPACITY_BYTES", "TORRENTFS_HTTP_MAX_UPLOAD_BYTES"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("load error = %q, want it to name %q", err, name)
		}
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

// TestLoadEnvironmentCredentialsOverrideTheConfigurationFile covers the
// precedence of the unified credentials: the environment wins over the
// configuration file, and the credentials are ordinary configuration keys.
func TestLoadEnvironmentCredentialsOverrideTheConfigurationFile(t *testing.T) {
	path := writeConfigFile(t, "username = \"file-user\"\npassword = \"file-password\"\n")
	got, err := load(path, lookupEnvironment(map[string]string{
		"TORRENTFS_USERNAME": "env-user",
		"TORRENTFS_PASSWORD": "password with spaces",
	}))
	if err != nil {
		t.Fatalf("load with environment credentials: %v", err)
	}
	if got.Username != "env-user" {
		t.Fatalf("username = %q, want the environment value", got.Username)
	}
	if got.Password != "password with spaces" {
		t.Fatalf("password = %q, want the environment value", got.Password)
	}
}

// TestLoadEnvironmentCredentialsRules covers the rules the unified
// credentials follow: the environment supplies them one at a time, and the
// requirements follow the selected output and the HTTP authentication.
func TestLoadEnvironmentCredentialsRules(t *testing.T) {
	tests := []struct {
		name         string
		config       string
		environment  map[string]string
		wantError    bool
		wantUsername string
		wantPassword string
	}{
		{
			name:         "the file supplies both credentials",
			config:       "backend = \"smb\"\nusername = \"file-user\"\npassword = \"file-password\"\n",
			wantUsername: "file-user",
			wantPassword: "file-password",
		},
		{
			name:         "the environment supplies only the user name",
			config:       "backend = \"smb\"\nusername = \"file-user\"\npassword = \"file-password\"\n",
			environment:  map[string]string{"TORRENTFS_USERNAME": "env-user"},
			wantUsername: "env-user",
			wantPassword: "file-password",
		},
		{
			name:         "the environment supplies only the password",
			config:       "backend = \"smb\"\nusername = \"file-user\"\npassword = \"file-password\"\n",
			environment:  map[string]string{"TORRENTFS_PASSWORD": "env-password"},
			wantUsername: "file-user",
			wantPassword: "env-password",
		},
		{
			name:      "SMB without any credential is rejected",
			config:    "backend = \"smb\"\n",
			wantError: true,
		},
		{
			name:      "SMB without a user name is rejected",
			config:    "backend = \"smb\"\npassword = \"a-password\"\n",
			wantError: true,
		},
		{
			name:      "SMB without a password is rejected",
			config:    "backend = \"smb\"\nusername = \"reader\"\n",
			wantError: true,
		},
		{
			name:   "the HTTP login needs no user name",
			config: "[http.auth]\nenabled = true\n",
			environment: map[string]string{
				"TORRENTFS_PASSWORD": "a-password",
			},
			wantPassword: "a-password",
		},
		{
			name:      "the HTTP login needs a password",
			config:    "[http.auth]\nenabled = true\n",
			wantError: true,
		},
		{
			name: "a disabled authentication does not need a password",
			environment: map[string]string{
				"TORRENTFS_USERNAME": "a-user",
				"TORRENTFS_PASSWORD": "a-password",
			},
			wantUsername: "a-user",
			wantPassword: "a-password",
		},
		{
			name: "the environment enables the authentication",
			environment: map[string]string{
				"TORRENTFS_HTTP_AUTH_ENABLED": "true",
				"TORRENTFS_PASSWORD":          "secret",
			},
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
			if got.Username != tt.wantUsername {
				t.Fatalf("username = %q, want %q", got.Username, tt.wantUsername)
			}
			if got.Password != tt.wantPassword {
				t.Fatalf("password = %q, want %q", got.Password, tt.wantPassword)
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
				"TORRENTFS_USERNAME":          "alice",
				"TORRENTFS_PASSWORD":          tt.password,
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
