package config

import (
	"errors"
	"sort"
	"strings"
)

// errUnknownEnvironment reports a variable in the configuration namespace that
// no configuration key derives.
//
// The validation report already names the variable in its field, so the
// sentence itself does not repeat the name.
var errUnknownEnvironment = errors.New("unknown environment variable in the " + environmentPrefix + " namespace; it is not a known configuration item")

// environmentException is a variable in the configuration namespace that the
// program accepts even though no configuration key derives it.
type environmentException struct {
	// Name is the variable name.
	Name string
}

// environmentExceptions lists the variables in the configuration namespace
// that configure no configuration key.
//
// The exceptions live in one table instead of an inline condition, so a new
// non-configuration variable joins the program by adding one entry and the
// unknown-variable rule stays one rule.
//
// The table holds the configuration file entry only: it names the file the
// configuration is read from, not a setting inside it. Everything that is not
// a configuration key lives outside the namespace: the test switches, the CI
// smoke knobs, and the Compose interpolation use the TF_ prefix, so the
// TORRENTFS_ namespace means configuration and nothing else.
var environmentExceptions = []environmentException{
	// The configuration file entry. It is known to the program, so the
	// unknown-variable rule must not report it.
	{Name: ConfigPathEnvironment},
}

// unknownEnvironmentNames reports the variables in the configuration
// namespace that configure nothing.
//
// A variable is known when a configuration key derives it, when a registered
// exception covers it, or when the removal table explains it. The removal
// table names the replacement of a removed variable, which helps the user
// more than the unknown-variable rule, so a removed name is left to it.
//
// The result is sorted, so the report reads the same way on every run.
func unknownEnvironmentNames(lookup envLookup) []string {
	known := make(map[string]bool, len(configSchema))
	for _, item := range configSchema {
		known[item.environment] = true
	}
	for _, removed := range removedNames {
		if removed.Kind == removedEnvironment {
			known[removed.Old] = true
		}
	}
	var unknown []string
	for _, name := range lookup.Names() {
		if !strings.HasPrefix(name, environmentPrefix) || known[name] {
			continue
		}
		if isEnvironmentException(name) {
			continue
		}
		unknown = append(unknown, name)
	}
	sort.Strings(unknown)
	return unknown
}

// isEnvironmentException reports whether a registered exception covers the
// given variable name.
func isEnvironmentException(name string) bool {
	for _, exception := range environmentExceptions {
		if name == exception.Name {
			return true
		}
	}
	return false
}

// unknownEnvironmentError reports the variables in the configuration namespace
// that configure nothing.
//
// Every unknown variable is reported, not just the first one: a user who
// misspelled two names learns about both at once. The variable name is the
// reported field, so the report names the variable that has to change.
func unknownEnvironmentError(lookup envLookup) error {
	unknown := unknownEnvironmentNames(lookup)
	if len(unknown) == 0 {
		return nil
	}
	collector := &validationCollector{}
	for _, name := range unknown {
		collector.add(invalid(name, errUnknownEnvironment))
	}
	return collector.err()
}
