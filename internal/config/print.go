package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
)

// redactedValue replaces the value of a secret configuration key in anything
// the program prints.
const redactedValue = "<redacted>"

// PrintOptions selects the form of the printed configuration.
type PrintOptions struct {
	// Sources asks for the source of every value alongside the value.
	Sources bool
}

// PrintedConfig is the printable form of the effective configuration.
type PrintedConfig struct {
	// Keys lists the configuration keys in the order they are printed.
	Keys []PrintedKey
	// Sources reports whether the source of every value is printed alongside
	// the value.
	Sources bool
}

// PrintedKey is one configuration key of the printed configuration.
type PrintedKey struct {
	// Path is the dot path of the configuration key.
	Path string
	// Environment is the environment variable that the path derives.
	Environment string
	// Value is the effective value of the configuration key, rendered for a
	// human. A secret key holds the redacted value instead.
	Value string
	// Source names the input the value came from.
	Source Source
	// Secret reports whether the value is a redacted one.
	Secret bool
	// Kind names the type of the value: string, bool, int, duration, byte
	// quantity, list, or enum.
	Kind string
}

// Summary returns the effective configuration as a log group.
//
// The group names every configuration key, its effective value and the input
// the value came from, so a user who asks why a setting did not take effect
// reads the answer out of the log instead of guessing. The secret values are
// redacted.
//
// The values are logged as text, because a log record is read by a person:
// the shape of the value matters less than the value itself.
func Summary(cfg Config, sources Sources) slog.Attr {
	attributes := make([]any, 0, len(configSchema))
	for _, item := range configSchema {
		value, ok := configurationValue(cfg, item.path)
		if !ok {
			continue
		}
		if item.secret {
			value = redactedValue
		}
		attributes = append(attributes, slog.Group(item.path,
			slog.String("value", value),
			slog.String("source", string(sources.SourceOf(item.path))),
		))
	}
	return slog.Group("config", attributes...)
}

// Print returns the effective configuration in a printable form.
//
// The values of the secret keys are redacted, so the result never discloses a
// password. The result is a report, not a configuration file: the redacted
// values are placeholders, so it is not meant to be read back.
func Print(path string, options PrintOptions) (PrintedConfig, error) {
	cfg, sources, err := LoadWithSources(path)
	if err != nil {
		return PrintedConfig{}, err
	}
	printed := PrintedConfig{}
	for _, key := range Schema() {
		value, ok := configurationValue(cfg, key.Path)
		if !ok {
			continue
		}
		entry := PrintedKey{
			Path:        key.Path,
			Environment: key.Environment,
			Value:       value,
			Source:      sources.SourceOf(key.Path),
			Secret:      key.Secret,
			Kind:        key.Kind,
		}
		if key.Secret {
			entry.Value = redactedValue
		}
		printed.Keys = append(printed.Keys, entry)
	}
	printed.Sources = options.Sources
	return printed, nil
}

// TOML renders the printed configuration as a TOML document.
//
// The source of a value is a comment, because TOML has no place for it. The
// redacted values are placeholders: the document is a report, not a
// configuration file.
func (p PrintedConfig) TOML() string {
	var builder strings.Builder
	section := ""
	for _, key := range p.Keys {
		name := lastSegment(key.Path)
		if parent := parentPath(key.Path); parent != section {
			section = parent
			if section != "" {
				if builder.Len() > 0 {
					builder.WriteString("\n")
				}
				builder.WriteString("[" + section + "]\n")
			}
		}
		builder.WriteString(fmt.Sprintf("%s = %s\n", name, tomlValue(key)))
		if p.Sources {
			builder.WriteString(fmt.Sprintf("# source: %s\n", key.Source))
		}
	}
	return builder.String()
}

// JSON renders the printed configuration as a JSON document.
//
// The source of a value sits beside the value, because JSON has no comment
// syntax.
func (p PrintedConfig) JSON() ([]byte, error) {
	values := map[string]any{}
	sources := map[string]string{}
	for _, key := range p.Keys {
		values[key.Path] = key.Value
		sources[key.Path] = string(key.Source)
	}
	document := map[string]any{"values": values}
	if p.Sources {
		document["sources"] = sources
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	// The escaped form of the redacted placeholder hides it from a reader:
	// "<redacted>" reads as a marker, "redacted" reads as noise.
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

// tomlValue renders one configuration key as a TOML value.
//
// The value keeps the type it has in the configuration, so a reader sees the
// same shape the configuration file uses. A redacted value is a string: the
// placeholder is not the value it replaces.
func tomlValue(key PrintedKey) string {
	if key.Secret {
		return fmt.Sprintf("%q", redactedValue)
	}
	switch key.Kind {
	case "bool":
		return key.Value
	case "int", "int64":
		return key.Value
	case "list":
		if key.Value == "" {
			return "[]"
		}
		items := strings.Split(key.Value, ", ")
		quoted := make([]string, 0, len(items))
		for _, item := range items {
			quoted = append(quoted, fmt.Sprintf("%q", item))
		}
		return "[" + strings.Join(quoted, ", ") + "]"
	default:
		return fmt.Sprintf("%q", key.Value)
	}
}

// configurationValue returns the effective value of the configuration key with
// the given path, rendered for a human.
func configurationValue(cfg Config, path string) (string, bool) {
	item, ok := schemaItemFor(path)
	if !ok {
		return "", false
	}
	value := reflectValueOf(cfg, item.index)
	if !value.IsValid() {
		return "", false
	}
	return renderValue(value), true
}

// schemaItemFor returns the schema entry of the configuration key with the
// given path.
func schemaItemFor(path string) (schemaItem, bool) {
	for _, item := range configSchema {
		if item.path == path {
			return item, true
		}
	}
	return schemaItem{}, false
}

// reflectValueOf returns the value of the field the index path points at.
func reflectValueOf(cfg Config, index []int) reflect.Value {
	return reflect.ValueOf(cfg).FieldByIndex(index)
}

// renderValue renders a configuration value for a human.
//
// The value is rendered the way the user would write it: a duration as a Go
// duration, a byte quantity with its unit, a list as its items.
func renderValue(value reflect.Value) string {
	// A named type knows how to write itself: a byte quantity writes its
	// unit, a duration writes its text. The check comes before the kind
	// switch, because a named integer is still an integer.
	if value.CanInterface() {
		if formatted, ok := value.Interface().(fmt.Stringer); ok {
			return formatted.String()
		}
	}
	switch value.Kind() {
	case reflect.String:
		return value.String()
	case reflect.Bool:
		return strconv.FormatBool(value.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(value.Int(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(value.Float(), 'f', -1, 64)
	case reflect.Slice:
		items := make([]string, 0, value.Len())
		for index := 0; index < value.Len(); index++ {
			items = append(items, value.Index(index).String())
		}
		return strings.Join(items, ", ")
	}
	return value.String()
}

// parentPath returns the section of a configuration key: everything before its
// last segment.
func parentPath(path string) string {
	index := strings.LastIndex(path, ".")
	if index < 0 {
		return ""
	}
	return path[:index]
}

// lastSegment returns the key name of a configuration key: everything after
// its last dot.
func lastSegment(path string) string {
	index := strings.LastIndex(path, ".")
	if index < 0 {
		return path
	}
	return path[index+1:]
}
