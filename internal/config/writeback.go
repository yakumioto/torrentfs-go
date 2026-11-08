package config

import (
	"fmt"
	"strings"
)

// WriteBack is the input of the configuration write-back renderer.
//
// It renders the sparse configuration file that replaces an existing one:
// the document holds the keys the file carried and the keys the caller
// changed. A key the file did not carry and the caller did not change keeps
// its built-in default and is left out, so the file stays as short as the
// user's configuration.
//
// The value of every key comes from File, the file configuration layer: the
// built-in defaults overlaid with the configuration file, before the
// environment is merged. A secret key is therefore rendered with the value
// the file carries, never with the environment value or the redacted
// placeholder of the printed configuration.
type WriteBack struct {
	// File is the file configuration layer the new file is rendered from.
	File Config
	// Sources records the input of every configuration key. A key whose
	// source is SourceFile is a key the configuration file carries. File must
	// be the file layer, not the effective configuration: the renderer has no
	// way to recover a file value the environment replaced.
	Sources Sources
	// Changed lists the configuration keys the caller changed. It is the
	// second set of keys the document holds.
	Changed []string
}

// TOML renders the new configuration file.
//
// The values come from the file configuration layer, not from the effective
// configuration: the environment never reaches the file. The keys keep the
// order of the generator, a table holds its own keys before the tables nested
// below it, and every key carries the documentation of the schema. Two calls
// with the same inputs render the same bytes.
func (w WriteBack) TOML() ([]byte, error) {
	included, err := w.includedKeys()
	if err != nil {
		return nil, err
	}

	var builder strings.Builder
	section := ""
	for _, key := range orderedKeys() {
		if !included[key.Path] {
			continue
		}
		item, ok := schemaItemFor(key.Path)
		if !ok {
			continue
		}
		value := reflectValueOf(w.File, item.index).Interface()
		if key.Secret {
			text, ok := value.(string)
			if !ok || text == "" || w.Sources.SourceOf(key.Path) != SourceFile {
				// An empty secret has nothing to preserve, and a value
				// that did not come from the file would move an
				// environment secret into the file. Neither is written.
				continue
			}
			if text == redactedValue {
				return nil, fmt.Errorf("config: write-back: %q holds the redacted placeholder, not the file value", key.Path)
			}
		}
		rendered, ok := renderConfigValue(item, value)
		if !ok {
			return nil, fmt.Errorf("config: write-back: cannot render the value of %q", key.Path)
		}
		documentation := key.Documentation
		if documentation == "" {
			return nil, fmt.Errorf("config: write-back: no documentation for %q", key.Path)
		}
		if parent := parentPath(key.Path); parent != section {
			section = parent
			if section != "" {
				builder.WriteString("\n[" + section + "]\n")
			}
		}
		writeKeyDocumentation(&builder, key, documentation)
		writeKeyLine(&builder, key.Path, rendered)
	}
	return []byte(builder.String()), nil
}

// includedKeys returns the configuration keys the rendered file holds: the
// keys the configuration file carries and the keys the caller changed.
func (w WriteBack) includedKeys() (map[string]bool, error) {
	included := make(map[string]bool, len(w.Sources)+len(w.Changed))
	for path, source := range w.Sources {
		if source == SourceFile {
			included[path] = true
		}
	}
	for _, path := range w.Changed {
		if _, ok := schemaItemFor(path); !ok {
			return nil, fmt.Errorf("config: write-back: unknown configuration key %q", path)
		}
		included[path] = true
	}
	return included, nil
}
