package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// TestImageDifferencesAreTheExpectedOnes covers the contract that the image
// configuration holds exactly the settings that differ from the built-in
// defaults: a reader sees what the container changes and nothing else.
func TestImageDifferencesAreTheExpectedOnes(t *testing.T) {
	want := map[string]string{
		"connections.listen_port":  "6881",
		"connections.disable_ipv6": "true",
	}
	differences := config.ImageDifferences()
	if len(differences) != len(want) {
		t.Fatalf("the image holds %d differences, want %d", len(differences), len(want))
	}
	for _, difference := range differences {
		value, ok := want[difference.Path]
		if !ok {
			t.Errorf("the image holds the unexpected difference %q", difference.Path)
			continue
		}
		if difference.Value != value {
			t.Errorf("the image sets %q to %q, want %q", difference.Path, difference.Value, value)
		}
	}
}

// TestImageOverlayDiffersFromTheDefaults covers the contract that the image
// configuration holds no setting that keeps its built-in value.
func TestImageOverlayDiffersFromTheDefaults(t *testing.T) {
	generator, err := config.NewGenerator()
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}
	document, err := generator.ImageOverlayTOML()
	if err != nil {
		t.Fatalf("ImageOverlayTOML: %v", err)
	}
	path := filepath.Join(t.TempDir(), "torrentfs.toml")
	if err := os.WriteFile(path, []byte(document), 0o644); err != nil {
		t.Fatalf("write the overlay: %v", err)
	}
	image, err := config.Load(path)
	if err != nil {
		t.Fatalf("the image configuration does not load: %v", err)
	}
	builtIn := config.Default()
	if image.Connections.ListenPort == builtIn.Connections.ListenPort {
		t.Error("the image binds the same peer port as the built-in default")
	}
	if image.Connections.DisableIPv6 == builtIn.Connections.DisableIPv6 {
		t.Error("the image has the same IPv6 setting as the built-in default")
	}
}

// TestComposeTemplateUsesTheCurrentNames covers the contract that the
// Compose template configures the program with the names it reads: a
// template that names a removed setting fails at the first start.
func TestComposeTemplateUsesTheCurrentNames(t *testing.T) {
	content, err := os.ReadFile("../../docker-compose.yaml")
	if err != nil {
		t.Fatalf("read the Compose template: %v", err)
	}
	template := string(content)
	for _, removed := range config.RemovedNames() {
		if removed.Kind != "environment" {
			continue
		}
		if strings.Contains(template, removed.Old) {
			t.Errorf("the Compose template names the removed variable %q", removed.Old)
		}
	}
	for _, name := range []string{"TORRENTFS_BACKEND", "TORRENTFS_USERNAME", "TORRENTFS_PASSWORD"} {
		if !strings.Contains(template, name) {
			t.Errorf("the Compose template does not name %q", name)
		}
	}
}

// TestNoRemovedNamesRemainInTheSource covers the contract that the removed
// names survive only in the removal table and in the migration table: a
// removed name elsewhere in the source is a name the program still answers
// to.
func TestNoRemovedNamesRemainInTheSource(t *testing.T) {
	entries, err := os.ReadDir("../../internal/config")
	if err != nil {
		t.Fatalf("read the configuration package: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		content, err := os.ReadFile(filepath.Join("../../internal/config", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		source := string(content)
		for _, removed := range config.RemovedNames() {
			if removed.Kind != "environment" {
				continue
			}
			// The removal table names the removed variables itself.
			if name == "removals.go" {
				continue
			}
			if !strings.Contains(source, removed.Old) {
				continue
			}
			t.Errorf("%s names the removed variable %q", name, removed.Old)
		}
	}
}
