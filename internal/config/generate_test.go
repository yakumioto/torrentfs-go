package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// TestGeneratedExampleLoads covers the contract that the generated example
// configuration is a configuration the program accepts.
func TestGeneratedExampleLoads(t *testing.T) {
	generator, err := config.NewGenerator()
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}
	document, err := generator.ExampleTOML()
	if err != nil {
		t.Fatalf("ExampleTOML: %v", err)
	}
	path := filepath.Join(t.TempDir(), "torrentfs.toml")
	if err := os.WriteFile(path, []byte(document), 0o644); err != nil {
		t.Fatalf("write the example: %v", err)
	}
	if _, err := config.Load(path); err != nil {
		t.Fatalf("the generated example does not load: %v", err)
	}
}

// TestGeneratedExampleHoldsEveryKey covers the contract that the example
// holds every configuration key: it is the reference a user reads, so a key
// that is missing from it is a key nobody can discover.
func TestGeneratedExampleHoldsEveryKey(t *testing.T) {
	generator, err := config.NewGenerator()
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}
	document, err := generator.ExampleTOML()
	if err != nil {
		t.Fatalf("ExampleTOML: %v", err)
	}
	for _, key := range config.Schema() {
		name := key.Path
		if index := strings.LastIndex(name, "."); index >= 0 {
			name = name[index+1:]
		}
		if !strings.Contains(document, name+" = ") {
			t.Errorf("the generated example does not hold %q", key.Path)
		}
	}
}

// TestGeneratedExampleHoldsTheDocumentation covers the contract that every
// configuration key reaches the user with its documentation.
func TestGeneratedExampleHoldsTheDocumentation(t *testing.T) {
	generator, err := config.NewGenerator()
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}
	for _, key := range config.Schema() {
		if generator.Documentation(key.Path) == "" {
			t.Errorf("the configuration key %q has no documentation", key.Path)
		}
	}
}

// TestGeneratedImageOverlayHoldsOnlyDifferences covers the contract that the
// image configuration holds the settings that differ from the built-in
// defaults, so a reader sees what the container changes.
func TestGeneratedImageOverlayHoldsOnlyDifferences(t *testing.T) {
	generator, err := config.NewGenerator()
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}
	document, err := generator.ImageOverlayTOML()
	if err != nil {
		t.Fatalf("ImageOverlayTOML: %v", err)
	}
	for _, key := range config.Schema() {
		name := key.Path
		if index := strings.LastIndex(name, "."); index >= 0 {
			name = name[index+1:]
		}
		difference := false
		for _, known := range config.ImageDifferences() {
			if known.Path == key.Path {
				difference = true
			}
		}
		if difference {
			continue
		}
		if strings.Contains(document, "\n"+name+" = ") {
			t.Errorf("the image configuration holds %q, which keeps its built-in value", key.Path)
		}
	}
}

// TestGeneratedImageOverlayLoads covers the contract that the image
// configuration is a configuration the program accepts.
func TestGeneratedImageOverlayLoads(t *testing.T) {
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
	if _, err := config.Load(path); err != nil {
		t.Fatalf("the generated image configuration does not load: %v", err)
	}
}

// TestReadmeReferenceHoldsEveryKey covers the contract that the reference in
// the README names every configuration key with its environment variable.
func TestReadmeReferenceHoldsEveryKey(t *testing.T) {
	generator, err := config.NewGenerator()
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}
	table := generator.ReferenceTable()
	for _, key := range config.Schema() {
		if !strings.Contains(table, "`"+key.Path+"`") {
			t.Errorf("the reference does not name %q", key.Path)
		}
		if !strings.Contains(table, "`"+key.Environment+"`") {
			t.Errorf("the reference does not name the environment variable of %q", key.Path)
		}
	}
}

// TestShippedConfigurationFilesMatchTheSchema covers the contract that the
// shipped configuration files are the generated ones: a hand edit to them is
// a change the generator does not know about.
func TestShippedConfigurationFilesMatchTheSchema(t *testing.T) {
	generator, err := config.NewGenerator()
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}
	cases := []struct {
		fragment string
		path     string
		document string
	}{
		{fragment: "example-toml", path: "../../torrentfs.example.toml"},
		{fragment: "image-overlay", path: "../../docker/torrentfs.toml"},
	}
	for _, tt := range cases {
		t.Run(tt.fragment, func(t *testing.T) {
			var document string
			switch tt.fragment {
			case "example-toml":
				document, err = generator.ExampleTOML()
			case "image-overlay":
				document, err = generator.ImageOverlayTOML()
			}
			if err != nil {
				t.Fatalf("generate %s: %v", tt.fragment, err)
			}
			shipped, err := os.ReadFile(tt.path)
			if err != nil {
				t.Fatalf("read %s: %v", tt.path, err)
			}
			if string(shipped) != document {
				t.Errorf("%s is not the generated %s; regenerate it", tt.path, tt.fragment)
			}
		})
	}
}
