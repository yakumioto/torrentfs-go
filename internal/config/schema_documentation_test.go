package config

import "testing"

// TestSchemaCarriesTheDocumentationOfEveryKey covers the documentation
// contract of the configuration page: every configuration key reaches a
// deployment without the source code with the documentation written above its
// field, so the page cannot show a blank or a stale description.
func TestSchemaCarriesTheDocumentationOfEveryKey(t *testing.T) {
	generator, err := NewGenerator()
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}
	documented := make(map[string]bool, len(schemaDocumentation))
	for path := range schemaDocumentation {
		documented[path] = true
	}
	for _, key := range Schema() {
		want := generator.Documentation(key.Path)
		if want == "" {
			t.Errorf("the configuration source documents no configuration key %q", key.Path)
			continue
		}
		if key.Documentation != want {
			t.Errorf("the embedded documentation of %q is not its source comment; regenerate the artifact", key.Path)
		}
		delete(documented, key.Path)
	}
	for path := range documented {
		t.Errorf("the embedded documentation holds the unknown configuration key %q", path)
	}
}

// TestSchemaDocumentationArtifactIsTheGeneratedOne covers the reproducibility
// of the embedded documentation: the artifact committed to the repository is
// byte for byte the one the generator writes, so a hand edit to it is a change
// the generator does not know about.
func TestSchemaDocumentationArtifactIsTheGeneratedOne(t *testing.T) {
	generator, err := NewGenerator()
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}
	generated, err := generator.SchemaDocumentationJSON()
	if err != nil {
		t.Fatalf("SchemaDocumentationJSON: %v", err)
	}
	if string(schemaDocumentationFile) != generated {
		t.Errorf("the embedded schema documentation is not the generated one; regenerate it with:\n" +
			"  go run ./internal/config/cmd/config-doc write-schema-documentation internal/config/schema-documentation.json")
	}
}
