package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Generator writes the documentation fragments of the configuration schema.
//
// The fragments are derived from the schema and from the comments of the
// configuration structure, so a configuration key cannot be documented in one
// place and missing in another.
type Generator struct {
	// comments holds the documentation of every configuration key.
	comments map[string]string
}

// NewGenerator reads the documentation of the configuration structure.
//
// The source files are the Go files of the configuration package: the
// comments live next to the setting they describe.
func NewGenerator() (*Generator, error) {
	files, err := sourceFiles()
	if err != nil {
		return nil, err
	}
	reader, err := readComments(files, "Config")
	if err != nil {
		return nil, err
	}
	return &Generator{comments: reader.comments}, nil
}

// Documentation returns the documentation of the configuration key with the
// given path.
func (g *Generator) Documentation(path string) string {
	return g.comments[path]
}

// ExampleTOML writes the complete example configuration.
//
// The example holds every configuration key with its default value and the
// documentation of the key, so it is the reference a user reads instead of
// the source code.
func (g *Generator) ExampleTOML() (string, error) {
	var builder strings.Builder
	builder.WriteString("# torrentfs.example.toml — the complete configuration of torrentfs.\n")
	builder.WriteString("#\n")
	builder.WriteString("# Every configuration key is listed here with its built-in default and its\n")
	builder.WriteString("# documentation. The environment variable of a key is derived from its path:\n")
	builder.WriteString("# the path in upper case, the dots replaced by underscores, under the\n")
	builder.WriteString("# TORRENTFS_ prefix. For example http.auth.token_ttl is\n")
	builder.WriteString("# TORRENTFS_HTTP_AUTH_TOKEN_TTL.\n")
	builder.WriteString("#\n")
	builder.WriteString("# The precedence is the environment variables, then this file, then the\n")
	builder.WriteString("# built-in defaults. A configuration file is optional: without one the\n")
	builder.WriteString("# built-in defaults apply.\n")

	section := ""
	emitted := map[string]bool{}
	for _, key := range orderedKeys() {
		parent := parentPath(key.Path)
		if parent != section {
			section = parent
			if section != "" && !emitted[section] {
				emitted[section] = true
				builder.WriteString("\n[" + section + "]\n")
			}
		}
		g.writeKey(&builder, key)
	}
	return builder.String(), nil
}

// orderedKeys returns the configuration keys in the order a configuration
// file writes them.
//
// A table holds the keys of its own section before the tables below it: TOML
// closes a table when the next one starts, so a key of the parent section
// that follows a sub-table belongs to the sub-table. The sub-tables follow
// their parent, because a reader looks for them nearby.
func orderedKeys() []SchemaKey {
	keys := Schema()
	sort.SliceStable(keys, func(i, j int) bool {
		return sectionOrder(keys[i].Path) < sectionOrder(keys[j].Path)
	})
	return keys
}

// sectionOrder returns the position of a key in the order of the sections.
//
// The position is the path of the section with the separator that sorts
// before any character, so a section comes before the sections below it and
// the sections keep the order of the schema among themselves.
func sectionOrder(path string) string {
	parent := parentPath(path)
	if parent == "" {
		return ""
	}
	return parent + "\x00"
}

// writeKey writes one configuration key with its documentation and value.
func (g *Generator) writeKey(builder *strings.Builder, key SchemaKey) {
	documentation := g.Documentation(key.Path)
	if documentation != "" {
		for _, line := range strings.Split(documentation, "\n") {
			builder.WriteString("# " + line + "\n")
		}
	}
	if key.Secret {
		builder.WriteString("# A secret value: prefer the environment variable " + key.Environment + ".\n")
	}
	value, ok := g.defaultValue(key.Path)
	if !ok {
		return
	}
	builder.WriteString(lastSegment(key.Path) + " = " + value + "\n")
}

// defaultValue renders the built-in default of a configuration key as a TOML
// value.
//
// The value keeps the shape the configuration file uses: a duration as a Go
// duration, a byte quantity with its unit, a list as an array.
func (g *Generator) defaultValue(path string) (string, bool) {
	item, ok := schemaItemFor(path)
	if !ok {
		return "", false
	}
	value, ok := SchemaDefault(path)
	if !ok {
		return "", false
	}
	return tomlDefault(item, value)
}

// tomlDefault renders a default value as a TOML value.
func tomlDefault(item schemaItem, value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return fmt.Sprintf("%q", typed), true
	case bool:
		return fmt.Sprintf("%t", typed), true
	case int:
		return fmt.Sprintf("%d", typed), true
	case int64:
		return fmt.Sprintf("%d", typed), true
	case []string:
		if len(typed) == 0 {
			return "[]", true
		}
		quoted := make([]string, 0, len(typed))
		for _, entry := range typed {
			quoted = append(quoted, fmt.Sprintf("%q", entry))
		}
		return "[" + strings.Join(quoted, ", ") + "]", true
	case Duration:
		return fmt.Sprintf("%q", typed.String()), true
	case ByteQuantity:
		return fmt.Sprintf("%q", typed.String()), true
	case OptionalByteQuantity:
		return fmt.Sprintf("%q", typed.String()), true
	case OptionalDuration:
		return fmt.Sprintf("%q", typed.String()), true
	case ByteRate:
		if typed == 0 {
			// A byte rate of zero has no unit to write; the empty value
			// means "not set".
			return `""`, true
		}
		return fmt.Sprintf("%q", formatByteRate(float64(typed))), true
	case Percent:
		return fmt.Sprintf("%q", formatPercent(float64(typed))), true
	case Ratio:
		return fmt.Sprintf("%q", formatRatio(float64(typed))), true
	}
	// A named type carries its own text: an enumeration writes its name, a
	// duration writes its text, a byte quantity writes its unit.
	if text, ok := value.(fmt.Stringer); ok {
		return fmt.Sprintf("%q", text.String()), true
	}
	return fmt.Sprintf("%q", fmt.Sprint(value)), true
}

// ImageOverlayTOML writes the configuration of the Docker image.
//
// The image holds the settings that differ from the built-in defaults: a
// container binds a fixed peer port and has no IPv6 in its network.
// Everything else keeps its built-in value, so the overlay stays short enough
// to read.
//
// The values of the image are written down here, next to the names of the
// settings they belong to, so the overlay and this list cannot disagree.
func (g *Generator) ImageOverlayTOML() (string, error) {
	var builder strings.Builder
	builder.WriteString("# torrentfs.toml — the configuration of the Docker image.\n")
	builder.WriteString("#\n")
	builder.WriteString("# The image holds the settings that differ from the built-in defaults.\n")
	builder.WriteString("# Everything else keeps its built-in value; this is not a complete\n")
	builder.WriteString("# configuration. The complete one is torrentfs.example.toml.\n")
	builder.WriteString("#\n")
	builder.WriteString("# Override a value with the environment variable of its path, or with a\n")
	builder.WriteString("# configuration file of your own mounted over this one.\n")

	section := ""
	for _, difference := range ImageDifferences() {
		parent := parentPath(difference.Path)
		if parent != section {
			section = parent
			if section != "" {
				builder.WriteString("\n[" + section + "]\n")
			}
		}
		documentation := g.Documentation(difference.Path)
		if documentation != "" {
			for _, line := range strings.Split(documentation, "\n") {
				builder.WriteString("# " + line + "\n")
			}
		}
		builder.WriteString(lastSegment(difference.Path) + " = " + difference.Value + "\n")
	}
	return builder.String(), nil
}

// schemaKeyFor returns the schema entry of the configuration key with the
// given path.
func schemaKeyFor(path string) (SchemaKey, bool) {
	for _, key := range Schema() {
		if key.Path == path {
			return key, true
		}
	}
	return SchemaKey{}, false
}

// ReferenceTable writes the configuration reference of the README.
//
// The table names every configuration key, its environment variable, its
// default and its documentation, so the README holds the whole configuration
// without repeating it by hand.
func (g *Generator) ReferenceTable() string {
	var builder strings.Builder
	builder.WriteString("| 配置项 | 环境变量 | 默认值 | 说明 |\n")
	builder.WriteString("| --- | --- | --- | --- |\n")
	for _, key := range Schema() {
		defaultValue, _ := g.defaultValue(key.Path)
		defaultValue = strings.Trim(defaultValue, "\"")
		documentation := g.Documentation(key.Path)
		documentation = strings.ReplaceAll(documentation, "\n", " ")
		builder.WriteString(fmt.Sprintf("| `%s` | `%s` | `%s` | %s |\n",
			key.Path, key.Environment, defaultValue, documentation))
	}
	return builder.String()
}

// ReadmeReferenceStart and ReadmeReferenceEnd mark the block of the README
// that holds the generated reference.
//
// The markers let the writer replace the block without touching the rest of
// the file, and the check finds the block without knowing where it is.
const (
	ReadmeReferenceStart = "<!-- BEGIN generated:readme-reference -->"
	ReadmeReferenceEnd   = "<!-- END generated:readme-reference -->"
)

// WriteReadmeReference replaces the generated block of the README with the
// reference table.
//
// The block is replaced rather than appended, so the README keeps its own
// text around the reference.
func WriteReadmeReference(path string) error {
	generator, err := NewGenerator()
	if err != nil {
		return err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read the README: %w", err)
	}
	start := strings.Index(string(content), ReadmeReferenceStart)
	end := strings.Index(string(content), ReadmeReferenceEnd)
	if start < 0 || end < 0 || end < start {
		return fmt.Errorf("the README has no generated reference block")
	}
	block := ReadmeReferenceStart + "\n" + generator.ReferenceTable() + ReadmeReferenceEnd
	updated := string(content[:start]) + block + string(content[end+len(ReadmeReferenceEnd):])
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("write the README: %w", err)
	}
	return nil
}

// ImageDifference is one setting of the Docker image whose value differs from
// the built-in default.
type ImageDifference struct {
	// Path is the dot path of the configuration key.
	Path string
	// Value is the value of the image, written as a TOML value.
	Value string
}

// ImageDifferences returns the settings whose image value differs from the
// built-in default.
//
// The image is a container: it binds a fixed peer port so the port can be
// published, and it has no IPv6 in its network. The difference is written
// down here instead of being read from the image configuration file, so the
// overlay and this list cannot disagree.
func ImageDifferences() []ImageDifference {
	return []ImageDifference{
		{Path: "connections.listen_port", Value: "6881"},
		{Path: "connections.disable_ipv6", Value: "true"},
	}
}
