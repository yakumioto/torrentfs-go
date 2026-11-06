package config

import (
	"fmt"
	"reflect"
	"strings"
)

// environmentPrefix is the prefix of every environment variable that
// configures torrentfs.
const environmentPrefix = "TORRENTFS_"

// SchemaKey describes one configuration key of the configuration schema.
type SchemaKey struct {
	// Path is the dot path of the configuration key, such as
	// "http.auth.token_ttl".
	Path string
	// Environment is the environment variable that the path derives. The
	// environment overrides the configuration key only when Overridable
	// reports true; otherwise an exception variable configures it, and
	// EnvironmentExceptions names that variable.
	Environment string
	// Overridable reports whether the environment may override the
	// configuration key.
	Overridable bool
	// Kind names the type of the value: string, bool, int, duration, byte
	// quantity, or list.
	Kind string
}

// Schema returns the configuration schema: every configuration key of the
// configuration, with the environment variable that derives from its path.
//
// The schema is derived from the configuration structure, so a configuration
// key is documented, overridden, and generated from one place.
func Schema() []SchemaKey {
	keys := make([]SchemaKey, 0, len(configSchema))
	for _, item := range configSchema {
		keys = append(keys, SchemaKey{
			Path:        item.path,
			Environment: item.environment,
			Overridable: item.overridable,
			Kind:        item.kind,
		})
	}
	return keys
}

// SchemaDefault returns the built-in default value of the configuration key
// with the given path. It reports false for a path the schema does not know.
func SchemaDefault(path string) (any, bool) {
	for _, item := range configSchema {
		if item.path != path {
			continue
		}
		return reflect.ValueOf(Default()).FieldByIndex(item.index).Interface(), true
	}
	return nil, false
}

// schemaItem is one registered configuration key. It carries everything the
// loader needs to read the value from the configuration and to write it from
// an environment value.
type schemaItem struct {
	// path is the dot path of the configuration key.
	path string
	// environment is the environment variable that the path derives.
	environment string
	// kind names the type of the value.
	kind string
	// index is the field index path from the root configuration.
	index []int
	// parse turns an environment value into a value of the field type. It is
	// nil for a type the environment cannot express.
	parse func(string) (any, error)
	// overridable reports whether the environment may override the key.
	overridable bool
}

// configSchema is the configuration schema of the configuration structure.
var configSchema = buildSchema(reflect.TypeOf(Config{}))

// buildSchema derives the configuration schema from a configuration
// structure. The TOML tags give the section names and the key names: a nested
// structure becomes a section and every leaf becomes one configuration key
// with a derived environment variable name.
func buildSchema(root reflect.Type) []schemaItem {
	var items []schemaItem
	collectSchema(root, nil, "", &items)
	return items
}

// collectSchema walks one structure and registers its leaves.
func collectSchema(structType reflect.Type, index []int, prefix string, items *[]schemaItem) {
	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		if field.PkgPath != "" {
			// An unexported field is not a configuration key.
			continue
		}
		name, ok := field.Tag.Lookup("toml")
		if !ok || name == "-" {
			// A field the TOML decoder ignores is not a configuration key.
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		fieldIndex := append(append([]int{}, index...), i)
		if field.Type.Kind() == reflect.Struct {
			collectSchema(field.Type, fieldIndex, path, items)
			continue
		}
		parse, kind, ok := environmentParser(field.Type)
		*items = append(*items, schemaItem{
			path:        path,
			environment: deriveEnvironmentName(path),
			kind:        kind,
			index:       fieldIndex,
			parse:       parse,
			overridable: ok && !environmentOwns(path),
		})
	}
}

// applySchema applies the environment values of the schema items to the
// configuration and reports the value of every configuration key the
// environment set.
func applySchema(items []schemaItem, root reflect.Value, lookup envLookup) (map[string]any, error) {
	values := make(map[string]any)
	for _, item := range items {
		if !item.overridable {
			continue
		}
		raw, ok := lookup.Lookup(item.environment)
		if !ok {
			continue
		}
		value, err := item.parse(raw)
		if err != nil {
			return values, invalid(item.path, fmt.Errorf("environment variable %s: %w", item.environment, err))
		}
		root.FieldByIndex(item.index).Set(reflect.ValueOf(value))
		values[item.path] = value
	}
	return values, nil
}

// deriveEnvironmentName derives the environment variable of a configuration
// key from its path: the path in upper case, with the dots replaced by
// underscores, under the TORRENTFS_ prefix.
func deriveEnvironmentName(path string) string {
	return environmentPrefix + strings.ToUpper(strings.ReplaceAll(path, ".", "_"))
}

// environmentParser returns the function that turns an environment value into
// a value of the given field type, together with the name of the kind. It
// reports false for a type the environment cannot express.
func environmentParser(fieldType reflect.Type) (func(string) (any, error), string, bool) {
	switch fieldType {
	case reflect.TypeOf(""):
		return func(raw string) (any, error) { return raw, nil }, "string", true
	case reflect.TypeOf(false):
		return func(raw string) (any, error) {
			value, err := parseEnvBool(raw)
			if err != nil {
				return nil, err
			}
			return value, nil
		}, "bool", true
	case reflect.TypeOf(int(0)):
		return func(raw string) (any, error) {
			value, err := parseEnvInt(raw)
			if err != nil {
				return nil, err
			}
			return value, nil
		}, "int", true
	case reflect.TypeOf(Duration(0)):
		return func(raw string) (any, error) {
			value, err := parseEnvDuration(raw)
			if err != nil {
				return nil, err
			}
			return value, nil
		}, "duration", true
	case reflect.TypeOf(ByteQuantity(0)):
		return func(raw string) (any, error) {
			value, err := ParseByteQuantity(raw)
			if err != nil {
				return nil, err
			}
			return ByteQuantity(value), nil
		}, "byte quantity", true
	case reflect.TypeOf([]string(nil)):
		return func(raw string) (any, error) { return parseEnvList(raw), nil }, "list", true
	}
	if fieldType.Kind() == reflect.Pointer && fieldType.Elem().Kind() == reflect.Bool {
		// A pointer to a boolean tells an unset value from false.
		return func(raw string) (any, error) {
			value, err := parseEnvBool(raw)
			if err != nil {
				return nil, err
			}
			return &value, nil
		}, "bool", true
	}
	return nil, "", false
}
