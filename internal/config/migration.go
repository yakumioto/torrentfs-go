package config

import "strings"

// The migration table is the README section that tells a user which
// configuration names no longer exist and what replaces them. It is rendered
// from the removal table, so a name cannot be removed from the program while
// it is still documented, or documented while the program still accepts it.
const (
	// migrationTableHeader is the first line of the table.
	migrationTableHeader = "| 已删除 | 替代写法 |"
	// migrationTableDivider separates the header from the rows.
	migrationTableDivider = "| --- | --- |"
	// migrationTableSeparator joins the replacements of one removed name.
	migrationTableSeparator = "、"
)

// MigrationTable renders the migration table of the README.
//
// The table is derived from the removal table, so the documentation and the
// program reject exactly the same names. The order is the order of the
// removal table, so adding a name to that table adds a row here.
//
// The result is a complete Markdown table: a header, a divider, and one row
// per removed name.
func MigrationTable() string {
	var table strings.Builder
	table.WriteString(migrationTableHeader)
	table.WriteString("\n")
	table.WriteString(migrationTableDivider)
	for _, removed := range removedNames {
		table.WriteString("\n")
		table.WriteString(migrationTableRow(removed))
	}
	return table.String()
}

// migrationTableRow renders one row of the migration table.
func migrationTableRow(removed RemovedName) string {
	replacements := make([]string, 0, len(removed.New))
	for _, name := range removed.New {
		replacements = append(replacements, "`"+name+"`")
	}
	return "| `" + removed.Old + "` | " + strings.Join(replacements, migrationTableSeparator) + " |"
}
