package config

import (
	"strings"
)

// documentKeyRows maps every leaf key of a configuration document to the line
// its value sits on.
//
// The TOML decoder reports the line of the value it rejected, not the key the
// value belongs to. A user needs the key: it is what they look up in the
// documentation. The document itself is the place that knows which key sits
// on which line.
type documentKeyRows map[string]int

// keyRows reads a configuration document and records the line of every leaf
// key.
//
// The scan reads the document as text rather than as TOML: the document was
// already parsed into a tree, and a line scanner needs only the keys and the
// lines they sit on. A key that spans several lines or repeats a table
// reports its last occurrence, which is the one the decoder rejects.
func keyRows(content []byte) documentKeyRows {
	rows := documentKeyRows{}
	section := ""
	for index, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			if end := strings.Index(trimmed, "]"); end > 0 {
				section = strings.Trim(trimmed[1:end], " \t\"")
			}
			continue
		}
		equals := strings.Index(trimmed, "=")
		if equals < 0 {
			continue
		}
		key := strings.Trim(strings.TrimSpace(trimmed[:equals]), "\"'")
		if key == "" {
			continue
		}
		if section != "" {
			key = section + "." + key
		}
		rows[key] = index + 1
	}
	return rows
}

// keyAt returns the configuration key whose value sits on the given line.
func (r documentKeyRows) keyAt(row int) string {
	if row <= 0 {
		return ""
	}
	for key, line := range r {
		if line == row {
			return key
		}
	}
	return ""
}
