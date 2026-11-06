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
//
// An exception covers either one variable or one family of variables, never
// both: a name covers a single variable, a prefix covers the variables that
// belong together.
type environmentException struct {
	// Name is the variable name. It is used when Prefix is empty.
	Name string
	// Prefix matches every variable that starts with it. It is used when
	// Name is empty.
	Prefix string
}

// matches reports whether the exception covers the given variable name.
func (e environmentException) matches(name string) bool {
	if e.Prefix != "" {
		return strings.HasPrefix(name, e.Prefix)
	}
	return name == e.Name
}

// environmentExceptions lists the variables in the configuration namespace
// that configure no configuration key.
//
// The exceptions live in one table instead of an inline condition, so a new
// non-configuration variable joins the program by adding one entry and the
// unknown-variable rule stays one rule.
var environmentExceptions = []environmentException{
	// The upload experiment knobs are research tooling with their own
	// namespace: they configure the experiment, not a configuration key.
	{Prefix: "TORRENTFS_LAB_"},
	// The smoke scripts and the CI jobs still use the configuration prefix
	// for their own switches. They move to the TF_ prefix, and this group
	// empties with them.
	{Prefix: "TORRENTFS_SMOKE_"},
	{Prefix: "TORRENTFS_RUNTIME_STATS_BROWSER_"},
	{Name: "TORRENTFS_FUSE_REQUIRED"},
	{Name: "TORRENTFS_SMB_SKIP_CIFS"},
	{Name: "TORRENTFS_TEST_UNAVAILABLE_FUSE_MOUNT"},
	{Name: "TORRENTFS_BIND_ADDRESS"},
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
		if exception.matches(name) {
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
