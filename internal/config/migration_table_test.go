package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// TestReadmeMigrationTableMatchesRemovalTable keeps the documented migration
// table and the names the program actually rejects from one source: a name
// that is removed from the code must also leave the README, and a name that
// is documented must be rejected at startup.
func TestReadmeMigrationTableMatchesRemovalTable(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	documented := documentedRemovedNames(string(readme))

	for _, removed := range config.RemovedNames() {
		if !documented[removed.Old] {
			t.Fatalf("removed name %q is not listed in the README migration table", removed.Old)
		}
	}
	for name := range documented {
		if !removedNameExists(name) {
			t.Fatalf("the README migration table lists %q, which the program does not reject", name)
		}
	}
}

// removedNameExists reports whether the program rejects the given name.
func removedNameExists(name string) bool {
	for _, removed := range config.RemovedNames() {
		if removed.Old == name {
			return true
		}
	}
	return false
}

// migrationTableRow matches one row of the README migration table.
var migrationTableRow = regexp.MustCompile(`^\|\s*` + "`([^`]+)`" + `\s*\|`)

// documentedRemovedNames returns the removed names the README migration table
// lists.
func documentedRemovedNames(readme string) map[string]bool {
	documented := make(map[string]bool)
	inTable := false
	for _, line := range strings.Split(readme, "\n") {
		if strings.HasPrefix(line, "| 已删除 | 替代写法 |") {
			inTable = true
			continue
		}
		if inTable {
			if !strings.HasPrefix(line, "|") {
				break
			}
			match := migrationTableRow.FindStringSubmatch(line)
			if match != nil {
				documented[match[1]] = true
			}
		}
	}
	return documented
}
