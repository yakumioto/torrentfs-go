package config

import (
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// envLookup reads configuration values from the process environment. It also
// enumerates the variables that are set, so a later stage can reject unknown
// configuration variables without a second environment source.
type envLookup interface {
	// Lookup returns the value of one variable and reports whether it is set.
	Lookup(name string) (string, bool)
	// Names returns the names of every variable that is set.
	Names() []string
}

// osEnvironment reads the process environment.
type osEnvironment struct{}

// Lookup returns the value of one environment variable.
func (osEnvironment) Lookup(name string) (string, bool) { return os.LookupEnv(name) }

// Names returns the names of every environment variable that is set.
func (osEnvironment) Names() []string {
	names := os.Environ()
	result := make([]string, 0, len(names))
	for _, entry := range names {
		if index := strings.IndexByte(entry, '='); index > 0 {
			result = append(result, entry[:index])
		}
	}
	return result
}

// applyEnvironment overlays the environment onto the configuration.
//
// The environment variable of a configuration key is derived from its path, so
// a configuration key needs no registration to become overridable. The
// applied values are recorded in sources, so a caller can tell which input
// won for every configuration key.
func applyEnvironment(cfg *Config, lookup envLookup, sources Sources) error {
	applied, err := applySchema(configSchema, reflect.ValueOf(cfg).Elem(), lookup)
	if err != nil {
		return err
	}
	for path := range applied {
		sources[path] = SourceEnvironment
	}
	return nil
}

// parseEnvList splits a comma-separated environment value. Blank entries are
// dropped so a trailing comma is not a configuration error.
func parseEnvList(raw string) []string {
	var values []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			values = append(values, item)
		}
	}
	return values
}

func parseEnvInt(raw string) (int, error) {
	value, err := strconv.ParseInt(raw, 10, 0)
	if err != nil {
		return 0, errors.New("must be a base-10 integer")
	}
	return int(value), nil
}

func parseEnvBool(raw string) (bool, error) {
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, errors.New("must be a boolean")
	}
	return value, nil
}

func parseEnvDuration(raw string) (Duration, error) {
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, errors.New("must be a Go duration")
	}
	return Duration(value), nil
}
