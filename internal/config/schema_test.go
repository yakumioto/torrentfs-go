package config

import (
	"reflect"
	"testing"
	"time"
)

// historicalEnvironmentCases lists every environment variable that the
// configuration has always supported, together with the configuration key it
// configures, the value a configuration file sets, and the value the
// environment sets.
//
// The table is the historical contract of the environment mapping: every name
// stays available, every name keeps configuring the same configuration key,
// and every name keeps winning over the configuration file. The two values
// differ on purpose, so a case also proves the precedence.
var historicalEnvironmentCases = []struct {
	// path is the configuration key the variable configures.
	path string
	// name is the environment variable the configuration has always used.
	name string
	// file is the configuration file that sets the key to a different value.
	file string
	// env is the environment value.
	env string
	// want is the effective value after the environment has been applied.
	want any
}{
	{
		path: "connections.listen_host",
		name: "TORRENTFS_CONNECTIONS_LISTEN_HOST",
		file: "[connections]\nlisten_host = \"0.0.0.0\"\n",
		env:  "127.0.0.1",
		want: "127.0.0.1",
	},
	{
		path: "connections.listen_port",
		name: "TORRENTFS_CONNECTIONS_LISTEN_PORT",
		file: "[connections]\nlisten_port = 1234\n",
		env:  "4321",
		want: 4321,
	},
	{
		path: "connections.disable_ipv4",
		name: "TORRENTFS_CONNECTIONS_DISABLE_IPV4",
		file: "[connections]\ndisable_ipv4 = true\n",
		env:  "false",
		want: false,
	},
	{
		path: "connections.disable_ipv6",
		name: "TORRENTFS_CONNECTIONS_DISABLE_IPV6",
		file: "[connections]\ndisable_ipv6 = true\n",
		env:  "false",
		want: false,
	},
	{
		path: "connections.no_port_forwarding",
		name: "TORRENTFS_CONNECTIONS_NO_PORT_FORWARDING",
		file: "[connections]\nno_port_forwarding = false\n",
		env:  "true",
		want: true,
	},
	{
		path: "connections.bootstrap_nodes",
		name: "TORRENTFS_CONNECTIONS_BOOTSTRAP_NODES",
		file: "[connections]\nbootstrap_nodes = [\"a.example:6881\"]\n",
		env:  " b.example:6881 , , c.example:6881 ,",
		want: []string{"b.example:6881", "c.example:6881"},
	},
	{
		path: "proxy.socks5_url",
		name: "TORRENTFS_PROXY_SOCKS5_URL",
		file: "[proxy]\nsocks5_url = \"socks5://proxy.example:1080\"\n",
		env:  "socks5h://other.example:1080",
		want: "socks5h://other.example:1080",
	},
	{
		path: "cache.capacity",
		name: "TORRENTFS_CACHE_CAPACITY",
		file: "[cache]\ncapacity = \"1MiB\"\n",
		env:  "2MiB",
		want: ByteQuantity(2 << 20),
	},
	{
		path: "identity.tracker_user_agent",
		name: "TORRENTFS_IDENTITY_TRACKER_USER_AGENT",
		file: "[identity]\ntracker_user_agent = \"file-agent\"\n",
		env:  "env-agent",
		want: "env-agent",
	},
	{
		path: "identity.peer_id_prefix",
		name: "TORRENTFS_IDENTITY_PEER_ID_PREFIX",
		file: "[identity]\npeer_id_prefix = \"-FILE0-\"\n",
		env:  "-ENV00-",
		want: "-ENV00-",
	},
	{
		path: "identity.extended_handshake_client_version",
		name: "TORRENTFS_IDENTITY_EXTENDED_HANDSHAKE_CLIENT_VERSION",
		file: "[identity]\nextended_handshake_client_version = \"file-version\"\n",
		env:  "env-version",
		want: "env-version",
	},
	{
		path: "http.listen_addr",
		name: "TORRENTFS_HTTP_LISTEN_ADDR",
		file: "[http]\nlisten_addr = \"127.0.0.1:8081\"\n",
		env:  "127.0.0.1:8082",
		want: "127.0.0.1:8082",
	},
	{
		path: "http.max_upload_size",
		name: "TORRENTFS_HTTP_MAX_UPLOAD_SIZE",
		file: "[http]\nmax_upload_size = \"1MiB\"\n",
		env:  "2MiB",
		want: ByteQuantity(2 << 20),
	},
	{
		path: "http.auth.enabled",
		name: "TORRENTFS_HTTP_AUTH_ENABLED",
		file: "[http.auth]\nenabled = true\n",
		env:  "false",
		want: false,
	},
	{
		path: "http.auth.token_ttl",
		name: "TORRENTFS_HTTP_AUTH_TOKEN_TTL",
		file: "[http.auth]\ntoken_ttl = \"1m\"\n",
		env:  "2m",
		want: Duration(2 * time.Minute),
	},
	{
		path: "strm.base_url",
		name: "TORRENTFS_STRM_BASE_URL",
		file: "[strm]\nbase_url = \"http://a.example:8080\"\n",
		env:  "http://b.example:8080",
		want: "http://b.example:8080",
	},
	{
		path: "log.level",
		name: "TORRENTFS_LOG_LEVEL",
		file: "[log]\nlevel = \"debug\"\n",
		env:  "warn",
		want: "warn",
	},
	{
		path: "log.format",
		name: "TORRENTFS_LOG_FORMAT",
		file: "[log]\nformat = \"json\"\n",
		env:  "text",
		want: "text",
	},
	{
		path: "log.add_source",
		name: "TORRENTFS_LOG_ADD_SOURCE",
		file: "[log]\nadd_source = true\n",
		env:  "false",
		want: false,
	},
	{
		path: "backend",
		name: "TORRENTFS_BACKEND",
		file: "backend = \"fuse\"\n",
		env:  "none",
		want: BackendNone,
	},
	{
		path: "filesystem.read_timeout",
		name: "TORRENTFS_FILESYSTEM_READ_TIMEOUT",
		file: "[filesystem]\nread_timeout = \"1s\"\n",
		env:  "2s",
		want: Duration(2 * time.Second),
	},
	{
		path: "fuse.mountpoint",
		name: "TORRENTFS_FUSE_MOUNTPOINT",
		file: "[fuse]\nmountpoint = \"/file-mount\"\n",
		env:  "/env-mount",
		want: "/env-mount",
	},
	{
		path: "fuse.strm_mountpoint",
		name: "TORRENTFS_FUSE_STRM_MOUNTPOINT",
		file: "[fuse]\nstrm_mountpoint = \"/file-strm\"\n",
		env:  "/env-strm",
		want: "/env-strm",
	},
	{
		path: "fuse.allow_other",
		name: "TORRENTFS_FUSE_ALLOW_OTHER",
		file: "[fuse]\nallow_other = false\n",
		env:  "true",
		want: true,
	},
	{
		path: "smb.listen_addr",
		name: "TORRENTFS_SMB_LISTEN_ADDR",
		file: "[smb]\nlisten_addr = \"127.0.0.1:1445\"\n",
		env:  "127.0.0.1:1446",
		want: "127.0.0.1:1446",
	},
	{
		path: "smb.username",
		name: "TORRENTFS_SMB_USERNAME",
		file: "[smb]\nusername = \"file-user\"\n",
		env:  "env-user",
		want: "env-user",
	},
	{
		path: "smb.password_file",
		name: "TORRENTFS_SMB_PASSWORD_FILE",
		file: "[smb]\npassword_file = \"/file/secret\"\n",
		env:  "/env/secret",
		want: "/env/secret",
	},
	{
		path: "smb.strm",
		name: "TORRENTFS_SMB_STRM",
		file: "[smb]\nstrm = \"on\"\n",
		env:  "off",
		want: STRMOff,
	},
}

// TestEnvironmentNamesAreDerivedFromConfigurationPaths covers the derivation
// rule: the environment variable of a configuration key is its path in upper
// case, with the dots replaced by underscores, under the TORRENTFS_ prefix.
// The name is unique, so one variable configures exactly one configuration key.
func TestEnvironmentNamesAreDerivedFromConfigurationPaths(t *testing.T) {
	seen := make(map[string]string)
	for _, key := range Schema() {
		if !key.Overridable {
			continue
		}
		want := deriveEnvironmentName(key.Path)
		if key.Environment != want {
			t.Fatalf("the configuration key %q uses the environment variable %q, want %q",
				key.Path, key.Environment, want)
		}
		if other, ok := seen[key.Environment]; ok {
			t.Fatalf("the environment variable %q configures both %q and %q",
				key.Environment, other, key.Path)
		}
		seen[key.Environment] = key.Path
	}
}

// TestSchemaCoversEveryHistoricalEnvironmentName covers the historical
// contract of the environment mapping: the configuration keys the environment
// overrides are exactly the configuration keys the historical environment
// variables configured, one to one.
func TestSchemaCoversEveryHistoricalEnvironmentName(t *testing.T) {
	overridable := make(map[string]string)
	for _, key := range Schema() {
		if key.Overridable {
			overridable[key.Path] = key.Environment
		}
	}
	for _, tt := range historicalEnvironmentCases {
		got, ok := overridable[tt.path]
		if !ok {
			t.Fatalf("the environment no longer overrides the configuration key %q", tt.path)
		}
		if got != tt.name {
			t.Fatalf("the configuration key %q uses the environment variable %q, want %q",
				tt.path, got, tt.name)
		}
		delete(overridable, tt.path)
	}
	for path := range overridable {
		t.Fatalf("the configuration key %q gained an environment variable that the configuration never had", path)
	}
}

// TestHistoricalEnvironmentNamesStillOverrideTheConfigurationFile covers the
// behavior of every historical environment variable: it configures the same
// configuration key as before and it wins over the configuration file.
func TestHistoricalEnvironmentNamesStillOverrideTheConfigurationFile(t *testing.T) {
	for _, tt := range historicalEnvironmentCases {
		t.Run(tt.path, func(t *testing.T) {
			cfg, err := load(writeConfigFile(t, tt.file), lookupEnvironment(map[string]string{
				tt.name: tt.env,
			}))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			got := configValue(t, &cfg, tt.path)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("%s = %#v, want %#v", tt.path, got, tt.want)
			}
		})
	}
}

// TestEnvironmentValueFormatsMatchTheConfigurationFile covers the value
// formats: a configuration key reads the same value whether the user writes
// it in the configuration file or in the environment.
func TestEnvironmentValueFormatsMatchTheConfigurationFile(t *testing.T) {
	for _, tt := range []struct {
		path string
		file string
		env  string
	}{
		{path: "connections.bootstrap_nodes", file: "[\"a.example:6881\", \"b.example:6881\"]", env: "a.example:6881,b.example:6881"},
		{path: "connections.listen_port", file: "6881", env: "6881"},
		{path: "connections.disable_ipv4", file: "true", env: "true"},
		{path: "cache.capacity", file: "\"1GiB\"", env: "1GiB"},
		{path: "http.max_upload_size", file: "\"10MiB\"", env: "10MiB"},
		{path: "http.auth.token_ttl", file: "\"1h30m\"", env: "1h30m"},
		{path: "filesystem.read_timeout", file: "\"45s\"", env: "45s"},
		{path: "log.add_source", file: "true", env: "true"},
	} {
		t.Run(tt.path, func(t *testing.T) {
			section, key := splitPath(tt.path)
			fromFile, err := load(writeConfigFile(t, "["+section+"]\n"+key+" = "+tt.file+"\n"),
				lookupEnvironment(nil))
			if err != nil {
				t.Fatalf("load the configuration file: %v", err)
			}
			fromEnvironment, err := load("", lookupEnvironment(map[string]string{
				deriveEnvironmentName(tt.path): tt.env,
			}))
			if err != nil {
				t.Fatalf("load the environment: %v", err)
			}
			want := configValue(t, &fromFile, tt.path)
			got := configValue(t, &fromEnvironment, tt.path)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s = %#v from the environment, want %#v from the configuration file",
					tt.path, got, want)
			}
		})
	}
}

// TestNewConfigurationKeyGetsEnvironmentOverride covers the promise that
// adding a configuration key needs no second registration: the schema derives
// the environment variable name and the parser of a key from its place in the
// configuration structure.
func TestNewConfigurationKeyGetsEnvironmentOverride(t *testing.T) {
	type section struct {
		Value   string   `toml:"value"`
		Count   int      `toml:"count"`
		Enabled bool     `toml:"enabled"`
		Nodes   []string `toml:"nodes"`
	}
	type structure struct {
		Section section `toml:"section"`
	}

	items := buildSchema(reflect.TypeOf(structure{}))
	names := make(map[string]string)
	for _, item := range items {
		names[item.path] = item.environment
	}
	for path, want := range map[string]string{
		"section.value":   "TORRENTFS_SECTION_VALUE",
		"section.count":   "TORRENTFS_SECTION_COUNT",
		"section.enabled": "TORRENTFS_SECTION_ENABLED",
		"section.nodes":   "TORRENTFS_SECTION_NODES",
	} {
		if got := names[path]; got != want {
			t.Fatalf("the configuration key %q uses the environment variable %q, want %q",
				path, got, want)
		}
	}

	cfg := structure{}
	applied, err := applySchema(items, reflect.ValueOf(&cfg).Elem(), lookupEnvironment(map[string]string{
		"TORRENTFS_SECTION_VALUE":   "hello",
		"TORRENTFS_SECTION_COUNT":   "7",
		"TORRENTFS_SECTION_ENABLED": "true",
		"TORRENTFS_SECTION_NODES":   "a.example:6881, b.example:6881",
	}))
	if err != nil {
		t.Fatalf("applySchema: %v", err)
	}
	if cfg.Section.Value != "hello" || cfg.Section.Count != 7 || !cfg.Section.Enabled {
		t.Fatalf("section = %+v, want the environment values", cfg.Section)
	}
	if !reflect.DeepEqual(cfg.Section.Nodes, []string{"a.example:6881", "b.example:6881"}) {
		t.Fatalf("section nodes = %v, want the environment list", cfg.Section.Nodes)
	}
	for _, path := range []string{"section.value", "section.count", "section.enabled", "section.nodes"} {
		if _, ok := applied[path]; !ok {
			t.Fatalf("the configuration key %q was not reported as applied", path)
		}
	}
}

// TestEnvironmentExceptionsAreExplicit covers the exceptions to the derivation
// rule: they are listed in one table, and every one of them says whether it
// configures a configuration key of its own.
func TestEnvironmentExceptionsAreExplicit(t *testing.T) {
	derived := make(map[string]string)
	for _, key := range Schema() {
		if key.Overridable {
			derived[key.Environment] = key.Path
		}
	}
	exceptions := EnvironmentExceptions()
	if len(exceptions) == 0 {
		t.Fatal("the exceptions to the derivation rule are not listed")
	}
	seen := make(map[string]bool)
	for _, exception := range exceptions {
		if seen[exception.Name] {
			t.Fatalf("the environment variable %q is listed twice", exception.Name)
		}
		seen[exception.Name] = true
		switch exception.Kind {
		case exceptionCredential:
			if exception.Path != "" {
				t.Fatalf("the credential variable %q must not claim a configuration key", exception.Name)
			}
			if _, ok := derived[exception.Name]; ok {
				t.Fatalf("the credential variable %q is a derived name", exception.Name)
			}
		case exceptionExcludedKey:
			if exception.Path == "" {
				t.Fatalf("the excluded variable %q must name its configuration key", exception.Name)
			}
			if exception.Name != deriveEnvironmentName(exception.Path) {
				t.Fatalf("the excluded variable %q is not the derived name of %q",
					exception.Name, exception.Path)
			}
			if _, ok := derived[exception.Name]; ok {
				t.Fatalf("the excluded variable %q still overrides its configuration key", exception.Name)
			}
		default:
			t.Fatalf("the exception %q has the unknown kind %q", exception.Name, exception.Kind)
		}
		if exception.Reason == "" {
			t.Fatalf("the exception %q has no reason", exception.Name)
		}
	}
}

// TestEnvironmentBackendIsAPlainConfigurationKey covers the consequence of
// the removed heuristic: the output backend is an ordinary configuration key,
// so its environment variable is a derived name and needs no exception.
func TestEnvironmentBackendIsAPlainConfigurationKey(t *testing.T) {
	for _, key := range Schema() {
		if key.Path != "backend" {
			continue
		}
		if key.Environment != "TORRENTFS_BACKEND" {
			t.Fatalf("the configuration key %q uses the environment variable %q, want %q",
				key.Path, key.Environment, "TORRENTFS_BACKEND")
		}
		if !key.Overridable {
			t.Fatalf("the configuration key %q is not overridable by the environment", key.Path)
		}
		return
	}
	t.Fatal("the schema has no configuration key \"backend\"")
}

// TestEnumeratedConfigurationKeysListTheirValues covers the enumerated
// configuration keys: the schema names the accepted values, so a user can
// discover them without reading the source code.
func TestEnumeratedConfigurationKeysListTheirValues(t *testing.T) {
	want := map[string][]string{
		"backend":  {"none", "fuse", "smb"},
		"smb.strm": {"auto", "on", "off"},
	}
	for _, key := range Schema() {
		values, ok := want[key.Path]
		if !ok {
			if key.Kind == "enum" {
				t.Fatalf("the configuration key %q is enumerated but the test does not cover it", key.Path)
			}
			continue
		}
		if key.Kind != "enum" {
			t.Fatalf("the configuration key %q has the kind %q, want enum", key.Path, key.Kind)
		}
		if !reflect.DeepEqual(key.Values, values) {
			t.Fatalf("the configuration key %q accepts %v, want %v", key.Path, key.Values, values)
		}
		delete(want, key.Path)
	}
	for path := range want {
		t.Fatalf("the schema has no enumerated configuration key %q", path)
	}
}

// TestEnvironmentBackendSelectionIsExplicit covers the side effect of the
// output backend variable: setting it selects the output backend, so a
// configured FUSE mountpoint does not select it instead.
func TestEnvironmentBackendSelectionIsExplicit(t *testing.T) {
	cfg, err := load("", lookupEnvironment(map[string]string{
		"TORRENTFS_BACKEND":         "none",
		"TORRENTFS_FUSE_MOUNTPOINT": "/env-mount",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Backend != BackendNone {
		t.Fatalf("backend = %q, want the explicit selection %q",
			cfg.Backend, BackendNone)
	}
}

// TestEnvironmentFUSEMountpointDoesNotSelectBackend covers the removed
// heuristic: a mountpoint no longer selects the FUSE output, so the output
// backend stays at its default until it is chosen explicitly.
func TestEnvironmentFUSEMountpointDoesNotSelectBackend(t *testing.T) {
	cfg, err := load("", lookupEnvironment(map[string]string{
		"TORRENTFS_FUSE_STRM_MOUNTPOINT": "/env-strm",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Backend != BackendNone {
		t.Fatalf("backend = %q, want the default %q",
			cfg.Backend, BackendNone)
	}
}

// TestEnvironmentExcludedKeysKeepTheirConfigurationFileValue covers the keys
// that the environment does not override: their value comes from the
// configuration file alone.
func TestEnvironmentExcludedKeysKeepTheirConfigurationFileValue(t *testing.T) {
	for _, exception := range EnvironmentExceptions() {
		if exception.Kind != exceptionExcludedKey {
			continue
		}
		t.Run(exception.Path, func(t *testing.T) {
			cfg, err := load(writeConfigFile(t, excludedKeyDocument(exception.Path)),
				lookupEnvironment(map[string]string{
					exception.Name: "environment-value",
				}))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			got := configValue(t, &cfg, exception.Path)
			if reflect.DeepEqual(got, "environment-value") {
				t.Fatalf("%s = %#v, want the configuration file value", exception.Path, got)
			}
		})
	}
}

// excludedKeyDocument writes a configuration file that sets an excluded
// configuration key to a value the validator accepts.
func excludedKeyDocument(path string) string {
	switch path {
	case "http.auth.username":
		return "[http.auth]\nenabled = true\nusername = \"file-user\"\npassword_hash = \"file-hash\"\n"
	case "http.auth.password_hash":
		return "[http.auth]\nenabled = true\nusername = \"file-user\"\npassword_hash = \"file-hash\"\n"
	case "http.auth.password_hash_file":
		return "[http.auth]\nenabled = true\nusername = \"file-user\"\npassword_hash_file = \"/file/hash\"\n"
	default:
		return ""
	}
}

// configValue returns the effective value of a configuration key.
func configValue(t *testing.T, cfg *Config, path string) any {
	t.Helper()
	for _, item := range configSchema {
		if item.path != path {
			continue
		}
		return reflect.ValueOf(cfg).Elem().FieldByIndex(item.index).Interface()
	}
	t.Fatalf("the schema has no configuration key %q", path)
	return nil
}

// splitPath splits a configuration key path into its section and its key.
func splitPath(path string) (string, string) {
	for index := len(path) - 1; index >= 0; index-- {
		if path[index] == '.' {
			return path[:index], path[index+1:]
		}
	}
	return "", path
}
