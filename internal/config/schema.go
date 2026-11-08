package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// environmentPrefix is the prefix of every environment variable that
// configures torrentfs.
const environmentPrefix = "TORRENTFS_"

// enumTag names the tag that lists the accepted values of an enumerated
// configuration key. The tag is the only place the values are written down.
const enumTag = "enum"

// secretTag names the tag that marks a configuration key whose value must
// not be disclosed. A secret key is redacted wherever a configuration value
// is printed.
const secretTag = "secret"

// componentTag names the tag that names the process component that owns a
// configuration key. The component is where a hot-applied value reaches the
// running process, so the configuration page can attribute and group the key
// without a second hand-maintained table.
const componentTag = "component"

// hotApplyTag names the tag that marks a configuration key whose new value
// can be applied to the running process without a restart. A key without the
// tag takes effect at the next start.
const hotApplyTag = "hotapply"

// SchemaKey describes one configuration key of the configuration schema.
type SchemaKey struct {
	// Path is the dot path of the configuration key, such as
	// "http.auth.token_ttl".
	Path string
	// Environment is the environment variable that the path derives. The
	// environment overrides the configuration key only when Overridable
	// reports true.
	Environment string
	// Overridable reports whether the environment may override the
	// configuration key.
	Overridable bool
	// Kind names the type of the value: string, bool, int, int64, duration,
	// byte quantity, byte rate, percent, ratio, list, or enum.
	Kind string
	// Values lists the accepted values of an enumerated configuration key.
	// It is empty for every other kind.
	Values []string
	// Secret reports whether the value of the configuration key must not be
	// disclosed. A caller that prints configuration values redacts the
	// secret ones.
	Secret bool
	// Component names the process component that owns the configuration key.
	// The component is where a hot-applied value is applied, so a key the
	// component cannot replace at run time is restart-required.
	Component string
	// HotApply reports whether a new value of the configuration key reaches
	// the running process without a restart. A key that does not report it
	// takes effect at the next start.
	HotApply bool
	// Documentation is the documentation of the configuration key, as written
	// above its field in the configuration structure. It comes from the
	// artifact embedded in the binary, so a deployment without the source
	// code can show the same documentation as the source tree.
	Documentation string
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
			Path:          item.path,
			Environment:   item.environment,
			Overridable:   item.overridable,
			Kind:          item.kind,
			Values:        item.values,
			Secret:        item.secret,
			Component:     item.component,
			HotApply:      item.hotApply,
			Documentation: schemaDocumentation[item.path],
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

// SchemaSecret reports whether the configuration key with the given path is a
// secret. It reports false for a path the schema does not know.
func SchemaSecret(path string) bool {
	for _, item := range configSchema {
		if item.path == path {
			return item.secret
		}
	}
	return false
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
	// values lists the accepted values of an enumerated configuration key.
	// It is empty for every other kind.
	values []string
	// secret reports whether the value of the key must not be disclosed.
	secret bool
	// component names the process component that owns the key.
	component string
	// hotApply reports whether the key reaches the running process without a
	// restart.
	hotApply bool
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
		values := enumValues(field)
		if len(values) > 0 {
			kind = "enum"
		}
		*items = append(*items, schemaItem{
			path:        path,
			environment: deriveEnvironmentName(path),
			kind:        kind,
			values:      values,
			secret:      isSecret(field),
			component:   field.Tag.Get(componentTag),
			hotApply:    isHotApply(field),
			index:       fieldIndex,
			parse:       parse,
			overridable: ok,
		})
	}
}

// enumValues returns the accepted values of an enumerated configuration key.
// An empty result means the key is not enumerated.
func enumValues(field reflect.StructField) []string {
	raw, ok := field.Tag.Lookup(enumTag)
	if !ok {
		return nil
	}
	return strings.Split(raw, ",")
}

// isSecret reports whether a configuration key is marked as a secret. Only the
// exact value "true" marks a key, so a mistyped tag never hides a value that
// the user expects to read.
func isSecret(field reflect.StructField) bool {
	return field.Tag.Get(secretTag) == "true"
}

// isHotApply reports whether a configuration key is hot-applicable. Only the
// exact value "true" marks a key, so a mistyped tag never promises a restart
// the component cannot avoid.
func isHotApply(field reflect.StructField) bool {
	return field.Tag.Get(hotApplyTag) == "true"
}

// schemaEnumValues returns the accepted values of the enumerated configuration
// key with the given path. It reports false for a key that is not enumerated.
//
// The schema is the only place the values are written down, so the validator
// and the environment parser report the same list.
func schemaEnumValues(path string) ([]string, bool) {
	for _, item := range configSchema {
		if item.path != path {
			continue
		}
		return item.values, len(item.values) > 0
	}
	return nil, false
}

// applySchema applies the environment values of the schema items to the
// configuration and reports the value of every configuration key the
// environment set.
//
// Every value that fails to parse is reported, not just the first one: a user
// who set several variables learns about all of them at once.
func applySchema(items []schemaItem, root reflect.Value, lookup envLookup) (map[string]any, error) {
	values := make(map[string]any)
	collector := &validationCollector{}
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
			collector.add(invalid(item.path, fmt.Errorf("environment variable %s: %w", item.environment, err)))
			continue
		}
		if err := item.accept(value); err != nil {
			collector.add(invalid(item.path, fmt.Errorf("environment variable %s: %w", item.environment, err)))
			continue
		}
		root.FieldByIndex(item.index).Set(reflect.ValueOf(value))
		values[item.path] = value
	}
	return values, collector.err()
}

// deriveEnvironmentName derives the environment variable of a configuration
// key from its path: the path in upper case, with the dots replaced by
// underscores, under the TORRENTFS_ prefix.
func deriveEnvironmentName(path string) string {
	return environmentPrefix + strings.ToUpper(strings.ReplaceAll(path, ".", "_"))
}

// accept reports whether a value is one of the accepted values of an
// enumerated configuration key. Every other kind accepts every value.
func (i schemaItem) accept(value any) error {
	if len(i.values) == 0 {
		return nil
	}
	text := reflect.ValueOf(value)
	if !text.IsValid() || text.Kind() != reflect.String {
		return nil
	}
	return acceptEnumValue(i.values, text.String())
}

// acceptEnumValue reports whether a value is one of the accepted values of an
// enumerated configuration key.
//
// An empty value is accepted when the key has an unset state: the user left
// the key out and the configuration decides what its absence means.
func acceptEnumValue(values []string, value string) error {
	if value == "" && allowsEmptyEnum(values) {
		return nil
	}
	for _, allowed := range values {
		if value == allowed {
			return nil
		}
	}
	return fmt.Errorf("must be one of %s", strings.Join(namedEnumValues(values), ", "))
}

// namedEnumValues returns the accepted values of an enumerated configuration
// key without the empty one. The empty value means "not configured", so it is
// not a value a user can write.
func namedEnumValues(values []string) []string {
	named := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		named = append(named, value)
	}
	return named
}

// allowsEmptyEnum reports whether an enumerated configuration key accepts an
// empty value.
//
// A key whose values are all set has no unset state, so an empty value is a
// mistake. A key that lists an empty value among its own has one: the empty
// value is written there to mean "not configured".
func allowsEmptyEnum(values []string) bool {
	for _, allowed := range values {
		if allowed == "" {
			return true
		}
	}
	return false
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
	case reflect.TypeOf(ByteRate(0)):
		return func(raw string) (any, error) {
			if strings.TrimSpace(raw) == "" {
				// The empty value is the "not set" state of a rate.
				return ByteRate(0), nil
			}
			value, err := ParseByteRate(raw)
			if err != nil {
				return nil, err
			}
			return ByteRate(value), nil
		}, "byte rate", true
	case reflect.TypeOf(Percent(0)):
		return func(raw string) (any, error) {
			if strings.TrimSpace(raw) == "" {
				return Percent(0), nil
			}
			value, err := ParsePercent(raw)
			if err != nil {
				return nil, err
			}
			return Percent(value), nil
		}, "percent", true
	case reflect.TypeOf(Ratio(0)):
		return func(raw string) (any, error) {
			if strings.TrimSpace(raw) == "" {
				return Ratio(0), nil
			}
			value, err := ParseRatio(raw)
			if err != nil {
				return nil, err
			}
			return Ratio(value), nil
		}, "ratio", true
	case reflect.TypeOf(int64(0)):
		return func(raw string) (any, error) {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("must be a signed integer")
			}
			return value, nil
		}, "int64", true
	case reflect.TypeOf(OptionalByteQuantity(0)):
		return func(raw string) (any, error) {
			if strings.TrimSpace(raw) == "" {
				return OptionalByteQuantity(0), nil
			}
			value, err := ParseByteQuantity(raw)
			if err != nil {
				return nil, err
			}
			return OptionalByteQuantity(value), nil
		}, "byte quantity", true
	case reflect.TypeOf(OptionalDuration(0)):
		return func(raw string) (any, error) {
			if strings.TrimSpace(raw) == "" {
				return OptionalDuration(0), nil
			}
			value, err := parseEnvDuration(raw)
			if err != nil {
				return nil, err
			}
			return OptionalDuration(value), nil
		}, "duration", true
	}
	if fieldType.Kind() == reflect.String {
		// A named string type carries the enumeration of its values; the
		// environment writes the same text the configuration file writes.
		return func(raw string) (any, error) {
			value := reflect.New(fieldType).Elem()
			value.SetString(raw)
			return value.Interface(), nil
		}, "string", true
	}
	return nil, "", false
}
