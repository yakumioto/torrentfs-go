package config

import "strings"

// ConfigPathEnvironment is the environment variable that names the
// configuration file the process loads.
//
// It is the one variable in the configuration namespace that configures no
// configuration key: it names the file the configuration is read from.
const ConfigPathEnvironment = "TORRENTFS_CONFIG"

// ResolveConfigPath returns the configuration file the process loads.
//
// The command-line argument wins over the environment variable, and the
// environment variable wins over loading no file at all. There is no
// implicit search path: a deployment that wants a configuration file says
// where it is, so a missing file stays a mistake the operator sees instead
// of a silent fallback to the built-in defaults.
//
// An empty environment value means "no configuration file", the same as an
// unset variable.
func ResolveConfigPath(flagPath string) string {
	return resolveConfigPath(flagPath, osEnvironment{})
}

// resolveConfigPath resolves the configuration file against a given
// environment.
func resolveConfigPath(flagPath string, lookup envLookup) string {
	if flagPath != "" {
		return flagPath
	}
	path, ok := lookup.Lookup(ConfigPathEnvironment)
	if !ok {
		return ""
	}
	return strings.TrimSpace(path)
}
