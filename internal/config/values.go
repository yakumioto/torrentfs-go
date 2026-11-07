package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

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

// configurationValue returns the effective value of the configuration key with
// the given path, rendered for a human.
func configurationValue(cfg Config, path string) (string, bool) {
	item, ok := schemaItemFor(path)
	if !ok {
		return "", false
	}
	value := reflect.ValueOf(cfg).FieldByIndex(item.index)
	if !value.IsValid() {
		return "", false
	}
	return renderValue(value), true
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
