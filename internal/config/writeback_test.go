package config

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// emptyEnvironment is an environment with no variables set. The write-back
// tests use it to load the file configuration layer: the environment is not
// part of a configuration file.
type emptyEnvironment struct{}

// Lookup reports that no variable is set.
func (emptyEnvironment) Lookup(string) (string, bool) { return "", false }

// Names reports that no variable is set.
func (emptyEnvironment) Names() []string { return nil }

// loadFileConfig loads the file configuration layer of a TOML document: the
// built-in defaults overlaid with the file, before the environment is merged.
func loadFileConfig(t *testing.T, document string) (Config, Sources) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "torrentfs.toml")
	if err := os.WriteFile(path, []byte(document), 0o644); err != nil {
		t.Fatalf("write the configuration file: %v", err)
	}
	file, sources, err := loadWithSources(path, emptyEnvironment{})
	if err != nil {
		t.Fatalf("load the file configuration: %v", err)
	}
	return file, sources
}

// TestWriteBackRendersOnlyTheFileKeysAndTheChangedKeys covers the sparse
// contract of the renderer: the new file holds the keys the old file carried
// and the keys the caller changed, and nothing else.
func TestWriteBackRendersOnlyTheFileKeysAndTheChangedKeys(t *testing.T) {
	file, sources := loadFileConfig(t, `username = "alice"

[cache]
capacity = "1GiB"

[http]
listen_addr = "127.0.0.1:9000"

[log]
level = "info"
format = "text"
`)
	file.Log.Level = "debug"

	document, err := WriteBack{
		File:    file,
		Sources: sources,
		Changed: []string{"log.level"},
	}.TOML()
	if err != nil {
		t.Fatalf("TOML: %v", err)
	}
	rendered := string(document)

	for _, want := range []string{
		`username = "alice"`,
		`capacity = "1GiB"`,
		`listen_addr = "127.0.0.1:9000"`,
		`level = "debug"`,
		`format = "text"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the rendered document does not hold %q:\n%s", want, rendered)
		}
	}
	for _, unwanted := range []string{
		"password =",
		"backend =",
		"read_grace =",
		"mountpoint =",
		"listen_port =",
	} {
		if strings.Contains(rendered, unwanted) {
			t.Errorf("the rendered document holds %q, which is neither a file key nor a changed key:\n%s", unwanted, rendered)
		}
	}
	for _, section := range []string{"[cache]", "[http]", "[log]"} {
		if !strings.Contains(rendered, section) {
			t.Errorf("the rendered document does not declare %s:\n%s", section, rendered)
		}
	}
	for _, absent := range []string{"[connections]", "[fuse]", "[identity]"} {
		if strings.Contains(rendered, absent) {
			t.Errorf("the rendered document declares the untouched section %s:\n%s", absent, rendered)
		}
	}
	if !strings.Contains(rendered, "# CapacityBytes is the maximum number of bytes retained in memory.") {
		t.Errorf("the rendered document does not hold the schema documentation:\n%s", rendered)
	}
}

// TestWriteBackNeverWritesASecretThatIsNotInTheFile covers the rule that the
// environment value of a secret never reaches the configuration file: the
// renderer keeps the secret of the file layer, not the effective
// configuration.
func TestWriteBackNeverWritesASecretThatIsNotInTheFile(t *testing.T) {
	file, sources := loadFileConfig(t, `username = "alice"

[smb]
strm = "auto"
`)
	// The effective configuration could carry an environment password. The
	// write-back must not turn it into a file value.
	file.Password = "environment-secret"
	file.SMB.STRM = STRMOff

	document, err := WriteBack{
		File:    file,
		Sources: sources,
		Changed: []string{"smb.strm", "password"},
	}.TOML()
	if err != nil {
		t.Fatalf("TOML: %v", err)
	}
	rendered := string(document)
	if strings.Contains(rendered, "environment-secret") {
		t.Errorf("the rendered document discloses the environment secret:\n%s", rendered)
	}
	if strings.Contains(rendered, "password =") {
		t.Errorf("the rendered document writes a password the file does not carry:\n%s", rendered)
	}
}

// TestWriteBackPreservesTheSecretOfTheConfigurationFile covers the rule that
// the renderer writes only the secret the file carries: a caller that tries
// to change the secret cannot overwrite it, and a save that touches another
// key keeps it.
func TestWriteBackPreservesTheSecretOfTheConfigurationFile(t *testing.T) {
	file, sources := loadFileConfig(t, `username = "alice"
password = "s3cr3t"

[smb]
strm = "auto"
`)
	file.SMB.STRM = STRMOff

	// The caller tries to change the secret. The renderer must keep the
	// value the file carries.
	document, err := WriteBack{
		File:    file,
		Sources: sources,
		Changed: []string{"smb.strm", "password"},
	}.TOML()
	if err != nil {
		t.Fatalf("TOML: %v", err)
	}
	rendered := string(document)
	if !strings.Contains(rendered, `password = "s3cr3t"`) {
		t.Errorf("the rendered document does not preserve the file secret:\n%s", rendered)
	}
	for _, unwanted := range []string{"<redacted>", `password = ""`} {
		if strings.Contains(rendered, unwanted) {
			t.Errorf("the rendered document holds %q instead of the file secret:\n%s", unwanted, rendered)
		}
	}
}

// TestWriteBackOmitsAnEmptySecret covers the rule that an empty secret is
// never written into a configuration file: an unset value has no secret to
// preserve, and a write-back must not invent one.
func TestWriteBackOmitsAnEmptySecret(t *testing.T) {
	file, sources := loadFileConfig(t, `username = "alice"
password = ""

[smb]
strm = "auto"
`)
	file.SMB.STRM = STRMOff

	document, err := WriteBack{
		File:    file,
		Sources: sources,
		Changed: []string{"smb.strm"},
	}.TOML()
	if err != nil {
		t.Fatalf("TOML: %v", err)
	}
	rendered := string(document)
	if strings.Contains(rendered, "password =") {
		t.Errorf("the rendered document writes an empty secret:\n%s", rendered)
	}
}

// TestWriteBackRoundTripsThroughTheStrictLoader covers the contract that the
// rendered file is a configuration file: the strict loader reads it back into
// the same file configuration layer, with the merged defaults and the secret
// intact.
func TestWriteBackRoundTripsThroughTheStrictLoader(t *testing.T) {
	file, sources := loadFileConfig(t, `backend = "fuse"
username = "alice"
password = "s3cr3t"

[cache]
capacity = "1GiB"

[connections]
listen_port = 6881
reading_conns = 32
uploading_conns = 4
read_grace = "90s"
disable_ipv6 = true
bootstrap_nodes = ["router.example:6881", "dht.example:6881"]

[fuse]
mountpoint = "/mnt/media"

[http]
listen_addr = "127.0.0.1:9000"
max_upload_size = "4MiB"

[http.auth]
enabled = true
token_ttl = "15m"

[identity]
tracker_user_agent = "torrentfs-test/1.0"

[lab]
upload_mode = "inject"
upload_rate = "1MiB/s"
upload_jitter = "12.5"

[log]
level = "info"
format = "json"

[smb]
strm = "off"

[strm]
base_url = "https://media.example/torrentfs/"
`)

	// The file layer after the caller applied its changes. The rendered file
	// must hold this configuration, including a key the file did not carry
	// before the change.
	file.Log.Level = "debug"
	file.Log.AddSource = true
	file.Cache.CapacityBytes = 2 << 30
	file.Connections.BootstrapNodes = []string{"router.example:6881", "dht.example:6881", "extra.example:6881"}
	file.Lab.UploadJitter = 20

	document, err := WriteBack{
		File:    file,
		Sources: sources,
		Changed: []string{"log.level", "log.add_source", "cache.capacity", "connections.bootstrap_nodes", "lab.upload_jitter"},
	}.TOML()
	if err != nil {
		t.Fatalf("TOML: %v", err)
	}

	path := filepath.Join(t.TempDir(), "torrentfs.toml")
	if err := os.WriteFile(path, document, 0o644); err != nil {
		t.Fatalf("write the rendered file: %v", err)
	}
	loaded, err := LoadWithEnvironment(path, emptyEnvironment{})
	if err != nil {
		t.Fatalf("the rendered file does not load:\n%s\n%v", document, err)
	}
	if !reflect.DeepEqual(loaded, file) {
		t.Fatalf("the rendered file loads to a different configuration:\n got %+v\nwant %+v\n%s", loaded, file, document)
	}
}

// TestWriteBackRejectsAnUnknownChangedKey covers the contract that a changed
// key the schema does not know is a mistake, not a key the renderer silently
// drops from the file.
func TestWriteBackRejectsAnUnknownChangedKey(t *testing.T) {
	file, sources := loadFileConfig(t, `username = "alice"
`)
	_, err := WriteBack{
		File:    file,
		Sources: sources,
		Changed: []string{"cache.capacityy"},
	}.TOML()
	if err == nil {
		t.Fatal("TOML accepted an unknown configuration key")
	}
	if !strings.Contains(err.Error(), "cache.capacityy") {
		t.Fatalf("TOML error = %q, want it to name the unknown key", err)
	}
}

// updateWriteBackGolden rewrites the golden documents instead of comparing
// them. The documents are checked byte for byte, so a renderer change that is
// intended must update them explicitly.
var updateWriteBackGolden = flag.Bool("update", false, "rewrite the write-back golden documents")

// TestWriteBackGolden covers the exact bytes of the rendered document: the
// sparse selection, the generated comments and the section order are part of
// the contract of the write-back, not an implementation detail.
func TestWriteBackGolden(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		apply   func(*Config)
		changed []string
	}{
		{
			name: "sparse",
			file: `username = "alice"

[cache]
capacity = "1GiB"

[http]
listen_addr = "127.0.0.1:9000"

[http.auth]
token_ttl = "45m"

[log]
level = "info"
format = "text"
`,
			apply:   func(file *Config) { file.Log.Level = "debug" },
			changed: []string{"log.level"},
		},
		{
			name: "secret",
			file: `username = "alice"
password = "s3cr3t"

[smb]
strm = "auto"
`,
			apply:   func(file *Config) { file.SMB.STRM = STRMOff },
			changed: []string{"smb.strm"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file, sources := loadFileConfig(t, tc.file)
			tc.apply(&file)
			document, err := WriteBack{
				File:    file,
				Sources: sources,
				Changed: tc.changed,
			}.TOML()
			if err != nil {
				t.Fatalf("TOML: %v", err)
			}
			path := filepath.Join("testdata", "writeback", tc.name+".golden.toml")
			if *updateWriteBackGolden {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatalf("create the golden directory: %v", err)
				}
				if err := os.WriteFile(path, document, 0o644); err != nil {
					t.Fatalf("write the golden document: %v", err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read the golden document: %v", err)
			}
			if !bytes.Equal(document, want) {
				t.Errorf("%s is not the rendered document; regenerate it with:\n"+
					"  go test ./internal/config -run TestWriteBackGolden -update -count=1", path)
			}
		})
	}
}

// TestWriteBackRoundTripsEverySchemaKey covers the round trip over the whole
// schema: the generated example carries every configuration key, and the
// rendered file reads back into the same file configuration layer.
func TestWriteBackRoundTripsEverySchemaKey(t *testing.T) {
	file, sources, err := loadWithSources(filepath.Join("..", "..", "torrentfs.example.toml"), emptyEnvironment{})
	if err != nil {
		t.Fatalf("load the example configuration: %v", err)
	}
	document, err := WriteBack{
		File:    file,
		Sources: sources,
	}.TOML()
	if err != nil {
		t.Fatalf("TOML: %v", err)
	}
	path := filepath.Join(t.TempDir(), "torrentfs.toml")
	if err := os.WriteFile(path, document, 0o644); err != nil {
		t.Fatalf("write the rendered file: %v", err)
	}
	loaded, err := LoadWithEnvironment(path, emptyEnvironment{})
	if err != nil {
		t.Fatalf("the rendered example does not load:\n%s\n%v", document, err)
	}
	if !reflect.DeepEqual(loaded, file) {
		t.Fatalf("the rendered example loads to a different configuration:\n got %+v\nwant %+v", loaded, file)
	}
}

// TestWriteBackRefusesTheRedactedSecretPlaceholder covers the safety rule
// that the printed placeholder is never written back as a password: a caller
// that hands the renderer the printed configuration instead of the file layer
// gets an error instead of a configuration file with a broken secret.
func TestWriteBackRefusesTheRedactedSecretPlaceholder(t *testing.T) {
	file, sources := loadFileConfig(t, `username = "alice"
password = "s3cr3t"
`)
	file.Password = redactedValue

	_, err := WriteBack{
		File:    file,
		Sources: sources,
	}.TOML()
	if err == nil {
		t.Fatal("TOML wrote the redacted secret placeholder")
	}
	if !strings.Contains(err.Error(), "redacted") {
		t.Fatalf("TOML error = %q, want it to name the redacted placeholder", err)
	}
}
