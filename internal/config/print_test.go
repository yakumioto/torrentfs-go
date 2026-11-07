package config_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

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

// TestPrintTOMLIsReadable covers the contract that the printed configuration
// has a TOML form that keeps the shape of the values.
func TestPrintTOMLIsReadable(t *testing.T) {
	printed, err := config.Print(writeConfig(t, "backend = \"none\"\n[connections]\nlisten_port = 12345\n"), config.PrintOptions{})
	if err != nil {
		t.Fatalf("Print: %v", err)
	}
	document := printed.TOML()
	for _, want := range []string{`backend = "none"`, "listen_port = 12345", "[connections]"} {
		if !strings.Contains(document, want) {
			t.Errorf("the printed configuration does not hold %q:\n%s", want, document)
		}
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
