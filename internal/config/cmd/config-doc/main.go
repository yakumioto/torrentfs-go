// Command torrentfs-config-doc writes the generated documentation fragments of
// the configuration schema.
//
// The fragments are derived from the same tables the program uses at runtime,
// so a name cannot be removed from the code while it is still documented.
// The command writes the fragments to standard output; the caller decides
// where they go.
//
// Usage:
//
//	go run ./internal/config/cmd/config-doc migration-table
//	go run ./internal/config/cmd/config-doc example-toml
//	go run ./internal/config/cmd/config-doc image-overlay
//	go run ./internal/config/cmd/config-doc schema-documentation
//	go run ./internal/config/cmd/config-doc write-schema-documentation <artifact>
//	go run ./internal/config/cmd/config-doc readme-reference
//	go run ./internal/config/cmd/config-doc write-readme-reference <readme>
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	fragment := os.Args[1]
	if fragment == "write-schema-documentation" {
		if len(os.Args) != 3 {
			usage()
			os.Exit(2)
		}
		if err := config.WriteSchemaDocumentation(os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "config-doc: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if fragment == "write-readme-reference" {
		if len(os.Args) != 3 {
			usage()
			os.Exit(2)
		}
		if err := config.WriteReadmeReference(os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "config-doc: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if fragment == "migration-table" {
		fmt.Println(config.MigrationTable())
		return
	}
	generator, err := config.NewGenerator()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config-doc: %v\n", err)
		os.Exit(1)
	}
	var document string
	switch fragment {
	case "example-toml":
		document, err = generator.ExampleTOML()
	case "image-overlay":
		document, err = generator.ImageOverlayTOML()
	case "schema-documentation":
		document, err = generator.SchemaDocumentationJSON()
	case "readme-reference":
		document = generator.ReferenceTable()
	default:
		fmt.Fprintf(os.Stderr, "config-doc: unknown fragment %q\n", fragment)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "config-doc: %v\n", err)
		os.Exit(1)
	}
	if !strings.HasSuffix(document, "\n") {
		document += "\n"
	}
	fmt.Print(document)
}

// usage reports the fragments the command writes.
func usage() {
	fmt.Fprintln(os.Stderr, "usage: config-doc <migration-table|example-toml|image-overlay|schema-documentation|write-schema-documentation|readme-reference|write-readme-reference>")
}
