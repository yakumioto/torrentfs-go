package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
)

// schemaDocumentationFile is the generated documentation of the configuration
// schema, embedded in the binary.
//
// The documentation lives in the comments of the configuration structure,
// which a Go program cannot read at run time. The generator writes the
// comments to this artifact, and the artifact is embedded so a deployment
// without the source code can show the same documentation as the source tree.
//
//go:embed schema-documentation.json
var schemaDocumentationFile []byte

// schemaDocumentation holds the documentation of every configuration key, by
// its dot path.
var schemaDocumentation = parseSchemaDocumentation(schemaDocumentationFile)

// SchemaDocumentationJSON writes the documentation of every configuration key
// as a JSON object that maps a path to its documentation.
//
// The rendered artifact is embedded by the configuration package, so the
// documentation and the schema reach the program together. The object is
// ordered by path, so a regenerated artifact changes only where the
// documentation changed.
func (g *Generator) SchemaDocumentationJSON() (string, error) {
	documentation := make(map[string]string, len(configSchema))
	for _, item := range configSchema {
		text := g.Documentation(item.path)
		if text == "" {
			return "", fmt.Errorf("the configuration key %q has no documentation", item.path)
		}
		documentation[item.path] = text
	}
	content, err := json.MarshalIndent(documentation, "", "  ")
	if err != nil {
		return "", fmt.Errorf("render the schema documentation: %w", err)
	}
	return string(content) + "\n", nil
}

// WriteSchemaDocumentation replaces the embedded documentation artifact at the
// given path with the one the generator writes.
//
// The artifact is embedded, so redirecting the generator output over the
// artifact would truncate the file before the generator is built. The command
// writes the file itself to keep the regeneration one step.
func WriteSchemaDocumentation(path string) error {
	generator, err := NewGenerator()
	if err != nil {
		return err
	}
	document, err := generator.SchemaDocumentationJSON()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(document), 0o644); err != nil {
		return fmt.Errorf("write the schema documentation: %w", err)
	}
	return nil
}

// parseSchemaDocumentation parses the embedded documentation artifact.
//
// The artifact is generated and checked in, so a malformed one is a broken
// build rather than a runtime condition: the Go tests and the generated-files
// check reject it before it ships.
func parseSchemaDocumentation(content []byte) map[string]string {
	if len(content) == 0 {
		// A shell opens and truncates the artifact before the generator that
		// imports this package is built, so an empty file is the moment of a
		// regeneration through a redirected output. Tolerating it keeps that
		// regeneration one step; an artifact that stays empty fails the Go
		// tests and the generated-files check.
		return map[string]string{}
	}
	var documentation map[string]string
	if err := json.Unmarshal(content, &documentation); err != nil {
		panic(fmt.Sprintf("config: the embedded schema documentation is invalid: %v", err))
	}
	return documentation
}
