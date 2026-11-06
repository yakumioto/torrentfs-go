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
package main

import (
	"fmt"
	"os"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: config-doc migration-table")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "migration-table":
		fmt.Println(config.MigrationTable())
	default:
		fmt.Fprintf(os.Stderr, "unknown fragment %q\n", os.Args[1])
		os.Exit(2)
	}
}
