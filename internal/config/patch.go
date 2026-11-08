package config

import (
	"errors"
	"fmt"
	"reflect"
)

// SetText assigns the text form of a configuration value to cfg.
//
// The text uses the same syntax as the environment variable of the key, so the
// configuration page and the environment accept the same values. The caller
// owns cfg: the page applies a patch to a copy of the file configuration and
// the copy is rewritten only when validation passes.
//
// A key the schema does not know, a secret key, and a value that does not
// parse are reported against the configuration key, so a caller can show the
// problem next to the field that caused it.
func SetText(cfg *Config, path, text string) error {
	item, ok := schemaItemFor(path)
	if !ok {
		return invalid(path, errors.New("unknown configuration key"))
	}
	if item.secret {
		return invalid(path, errors.New("a secret configuration key cannot be edited"))
	}
	if item.parse == nil {
		return invalid(path, errors.New("the configuration key has no text form"))
	}
	value, err := item.parse(text)
	if err != nil {
		return invalid(path, err)
	}
	reflect.ValueOf(cfg).Elem().FieldByIndex(item.index).Set(reflect.ValueOf(value))
	return nil
}

// MergeKeys copies the value of every configuration key in paths from src to
// dst.
//
// The hot-apply boundary uses it: the running configuration adopts the values
// of the keys that took effect and keeps the previous values of the keys that
// wait for a restart. A slice value is copied, so the two configurations never
// alias one.
func MergeKeys(dst *Config, src Config, paths []string) error {
	for _, path := range paths {
		item, ok := schemaItemFor(path)
		if !ok {
			return fmt.Errorf("config: unknown configuration key %q", path)
		}
		value := reflect.ValueOf(src).FieldByIndex(item.index)
		if value.Kind() == reflect.Slice {
			cloned := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
			reflect.Copy(cloned, value)
			value = cloned
		}
		reflect.ValueOf(dst).Elem().FieldByIndex(item.index).Set(value)
	}
	return nil
}
