package config_test

import (
	"sort"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// testEnvironment is an environment the configuration can read.
//
// It is the seam every configuration test drives the loader through: a test
// names the variables it cares about and the configuration sees exactly
// those.
type testEnvironment map[string]string

// The environment satisfies the seam of the configuration package.
var _ config.Environment = testEnvironment{}

// Lookup returns the value of one environment variable.
func (e testEnvironment) Lookup(name string) (string, bool) {
	value, ok := e[name]
	return value, ok
}

// Names returns the names of every environment variable that is set.
func (e testEnvironment) Names() []string {
	names := make([]string, 0, len(e))
	for name := range e {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// lookupEnvironment injects a fixed set of environment variables.
func lookupEnvironment(values map[string]string) testEnvironment {
	return testEnvironment(values)
}
