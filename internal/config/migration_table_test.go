package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// TestReadmeMigrationTableMatchesRemovalTable keeps the documented migration
// table and the names the program actually rejects from one source: the table
// in the README is the one the removal table renders, so a name that is
// removed from the code also leaves the README, and a name that is
// documented is rejected at startup.
//
// The README section is compared with the rendered table instead of being
// parsed: a row that drifts from the removal table fails the comparison, and
// a name the program does not reject cannot appear in it.
func TestReadmeMigrationTableMatchesRemovalTable(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	want := config.MigrationTable()
	got := readmeMigrationTable(string(readme))
	if got == "" {
		t.Fatal("the README has no migration table")
	}
	if got != want {
		t.Fatalf("the README migration table does not match the removal table:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// readmeMigrationTable returns the migration table of the README.
func readmeMigrationTable(readme string) string {
	lines := strings.Split(readme, "\n")
	start := -1
	for index, line := range lines {
		if line == "| 已删除 | 替代写法 |" {
			start = index
			break
		}
	}
	if start < 0 {
		return ""
	}
	var table []string
	for _, line := range lines[start:] {
		if !strings.HasPrefix(line, "|") {
			break
		}
		table = append(table, line)
	}
	return strings.Join(table, "\n")
}

// TestMigrationTableNamesEveryRemovedName checks that the rendered table
// lists every removed name, so a name the program rejects cannot be missing
// from the documentation.
func TestMigrationTableNamesEveryRemovedName(t *testing.T) {
	table := config.MigrationTable()
	for _, removed := range config.RemovedNames() {
		if !strings.Contains(table, "`"+removed.Old+"`") {
			t.Fatalf("removed name %q is missing from the migration table", removed.Old)
		}
		for _, replacement := range removed.New {
			if !strings.Contains(table, "`"+replacement+"`") {
				t.Fatalf("the migration table does not name the replacement %q of %q", replacement, removed.Old)
			}
		}
	}
}

// TestMigrationTableIsDerivedFromTheRemovalTable checks that the rendered
// table has one row per removed name and no other row.
func TestMigrationTableIsDerivedFromTheRemovalTable(t *testing.T) {
	lines := strings.Split(config.MigrationTable(), "\n")
	rows := 0
	for _, line := range lines {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		rows++
	}
	if rows != len(config.RemovedNames()) {
		t.Fatalf("the migration table has %d rows, want %d", rows, len(config.RemovedNames()))
	}
}
