package config_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// TestPrintRedactsTheSecret covers the contract that the printed
// configuration never discloses a password.
func TestPrintRedactsTheSecret(t *testing.T) {
	const secret = "a-password-that-must-not-leak"
	path := writeConfig(t, "username = \"reader\"\npassword = \""+secret+"\"\n")

	printed, err := config.Print(path, config.PrintOptions{})
	if err != nil {
		t.Fatalf("Print: %v", err)
	}
	for _, key := range printed.Keys {
		if strings.Contains(key.Value, secret) {
			t.Fatalf("the printed configuration disclosed the password: %+v", key)
		}
	}
	var found bool
	for _, key := range printed.Keys {
		if key.Path == "password" {
			found = true
			if key.Value != "<redacted>" {
				t.Errorf("the password is printed as %q, want <redacted>", key.Value)
			}
			if !key.Secret {
				t.Error("the password is not marked as a secret")
			}
		}
	}
	if !found {
		t.Error("the printed configuration does not name the password")
	}
}

// TestPrintReportsEveryKey covers the contract that the printed
// configuration holds every configuration key, not only the ones that were
// set.
func TestPrintReportsEveryKey(t *testing.T) {
	printed, err := config.Print(writeConfig(t, "[log]\nlevel = \"debug\"\n"), config.PrintOptions{})
	if err != nil {
		t.Fatalf("Print: %v", err)
	}
	paths := make(map[string]bool, len(printed.Keys))
	for _, key := range printed.Keys {
		paths[key.Path] = true
	}
	for _, key := range config.Schema() {
		if !paths[key.Path] {
			t.Errorf("the printed configuration does not name %q", key.Path)
		}
	}
}

// TestPrintNamesTheSource covers the contract that the printed configuration
// names the input every value came from.
func TestPrintNamesTheSource(t *testing.T) {
	printed, err := config.Print(writeConfig(t, "[connections]\nlisten_port = 12345\n"), config.PrintOptions{Sources: true})
	if err != nil {
		t.Fatalf("Print: %v", err)
	}
	for _, key := range printed.Keys {
		want := config.SourceDefault
		switch key.Path {
		case "connections.listen_port":
			want = config.SourceFile
		}
		if key.Source != want {
			t.Errorf("%s comes from %q, want %q", key.Path, key.Source, want)
		}
	}
}

// TestPrintTOMLIsAValidDocument covers the contract that the TOML form of the
// report is a document a TOML parser accepts: a section is declared once,
// wherever its keys sit in the schema.
func TestPrintTOMLIsAValidDocument(t *testing.T) {
	printed, err := config.Print(writeConfig(t, "backend = \"none\"\n[connections]\nlisten_port = 12345\n"), config.PrintOptions{Sources: true})
	if err != nil {
		t.Fatalf("Print: %v", err)
	}
	document := printed.TOML()

	var decoded map[string]any
	if err := toml.Unmarshal([]byte(document), &decoded); err != nil {
		t.Fatalf("the report is not a TOML document: %v\n%s", err, document)
	}
	if decoded["backend"] != "none" {
		t.Errorf("backend = %v, want none", decoded["backend"])
	}
	httpSection, ok := decoded["http"].(map[string]any)
	if !ok {
		t.Fatalf("the report has no [http] table:\n%s", document)
	}
	if _, ok := httpSection["max_upload_size"]; !ok {
		t.Errorf("the [http] table does not hold max_upload_size:\n%s", document)
	}
	// The nested table proves both halves of the bug: the section of a
	// parent key arrives after its child table in the schema, so the parent
	// must not be declared twice.
	authSection, ok := httpSection["auth"].(map[string]any)
	if !ok {
		t.Fatalf("the [http] table does not hold [http.auth]:\n%s", document)
	}
	if _, ok := authSection["token_ttl"]; !ok {
		t.Errorf("[http.auth] does not hold token_ttl:\n%s", document)
	}
}

// TestTOMLKeepsTheRootKeysFirst covers the contract that a root key stays
// parseable wherever it sits in the report: TOML forbids a top-level key
// after a table header.
func TestTOMLKeepsTheRootKeysFirst(t *testing.T) {
	printed := config.PrintedConfig{Keys: []config.PrintedKey{
		{Path: "connections.listen_port", Environment: "TORRENTFS_CONNECTIONS_LISTEN_PORT", Value: "12345", Kind: "int"},
		{Path: "backend", Environment: "TORRENTFS_BACKEND", Value: "none", Kind: "string"},
		{Path: "http.listen_addr", Environment: "TORRENTFS_HTTP_LISTEN_ADDR", Value: "127.0.0.1:8080", Kind: "string"},
	}}

	document := printed.TOML()
	var decoded map[string]any
	if err := toml.Unmarshal([]byte(document), &decoded); err != nil {
		t.Fatalf("the report is not a TOML document: %v\n%s", err, document)
	}
	if decoded["backend"] != "none" {
		t.Errorf("backend = %v, want none\n%s", decoded["backend"], document)
	}
	connections, ok := decoded["connections"].(map[string]any)
	if !ok || connections["listen_port"] != int64(12345) {
		t.Errorf("[connections] lost listen_port:\n%s", document)
	}
}

// TestPrintJSONIsReadable covers the contract that the printed configuration
// has a JSON form whose values are the effective values.
func TestPrintJSONIsReadable(t *testing.T) {
	printed, err := config.Print(writeConfig(t, "backend = \"none\"\n"), config.PrintOptions{Sources: true})
	if err != nil {
		t.Fatalf("Print: %v", err)
	}
	document, err := printed.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var decoded struct {
		Values  map[string]string `json:"values"`
		Sources map[string]string `json:"sources"`
	}
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatalf("the printed configuration is not JSON: %v", err)
	}
	if decoded.Values["backend"] != "none" {
		t.Errorf("backend = %q, want none", decoded.Values["backend"])
	}
	if decoded.Sources["backend"] != "file" {
		t.Errorf("the source of backend is %q, want file", decoded.Sources["backend"])
	}
	if decoded.Sources["cache.capacity"] != "default" {
		t.Errorf("the source of cache.capacity is %q, want default", decoded.Sources["cache.capacity"])
	}
}

// TestSummaryRedactsTheSecret covers the contract that the startup log does
// not disclose a password.
func TestSummaryRedactsTheSecret(t *testing.T) {
	const secret = "a-password-that-must-not-leak"
	cfg, sources, err := config.LoadWithSources(writeConfig(t, "password = \""+secret+"\"\n"))
	if err != nil {
		t.Fatalf("LoadWithSources: %v", err)
	}
	var buffer bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buffer, nil))
	logger.Info("effective configuration", config.Summary(cfg, sources))
	if strings.Contains(buffer.String(), secret) {
		t.Fatalf("the log disclosed the password: %s", buffer.String())
	}
	if !strings.Contains(buffer.String(), "<redacted>") {
		t.Errorf("the log does not mark the redacted value: %s", buffer.String())
	}
	if !strings.Contains(buffer.String(), "backend") {
		t.Errorf("the log does not name the configuration keys: %s", buffer.String())
	}
}

// TestValueTextRendersAValueForTheConfigurationPage covers the value text the
// configuration page shows: a person reads the value, so it has the shape the
// configuration file uses, not a TOML quoted literal.
func TestValueTextRendersAValueForTheConfigurationPage(t *testing.T) {
	cfg := config.Default()
	cfg.Cache.CapacityBytes = config.ByteQuantity(32 * 1000 * 1000)
	cfg.Log.Level = "debug"
	cfg.Log.AddSource = true
	cfg.Connections.ListenPort = 51413

	tests := []struct {
		path string
		want string
	}{
		{"cache.capacity", "32MB"},
		{"log.level", "debug"},
		{"log.add_source", "true"},
		{"connections.listen_port", "51413"},
		{"http.auth.token_ttl", "30m0s"},
	}
	for _, test := range tests {
		got, ok := config.ValueText(cfg, test.path)
		if !ok {
			t.Errorf("ValueText(%q) reports an unknown key", test.path)
			continue
		}
		if got != test.want {
			t.Errorf("ValueText(%q) = %q, want %q", test.path, got, test.want)
		}
	}

	if _, ok := config.ValueText(cfg, "no.such.key"); ok {
		t.Error("ValueText reports an unknown key as known")
	}
}
