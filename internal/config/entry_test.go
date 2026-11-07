package config

import (
	"testing"
)

// TestResolveConfigPathPrefersTheFlag covers the precedence of the
// configuration file entry: the command-line argument wins over the
// environment variable.
func TestResolveConfigPathPrefersTheFlag(t *testing.T) {
	lookup := lookupEnvironment(map[string]string{
		ConfigPathEnvironment: "/etc/torrentfs/environment.toml",
	})
	got := resolveConfigPath("/etc/torrentfs/flag.toml", lookup)
	if got != "/etc/torrentfs/flag.toml" {
		t.Fatalf("resolveConfigPath = %q, want the flag path", got)
	}
}

// TestResolveConfigPathUsesTheEnvironmentVariable covers the second step of
// the precedence: without a command-line argument the environment variable
// names the configuration file.
func TestResolveConfigPathUsesTheEnvironmentVariable(t *testing.T) {
	lookup := lookupEnvironment(map[string]string{
		ConfigPathEnvironment: "/etc/torrentfs/environment.toml",
	})
	got := resolveConfigPath("", lookup)
	if got != "/etc/torrentfs/environment.toml" {
		t.Fatalf("resolveConfigPath = %q, want the environment path", got)
	}
}

// TestResolveConfigPathHasNoImplicitSearchPath covers the third step: with
// neither an argument nor the environment variable, no configuration file is
// loaded. The program never guesses a path, so a missing file stays a
// mistake the operator sees.
func TestResolveConfigPathHasNoImplicitSearchPath(t *testing.T) {
	got := resolveConfigPath("", lookupEnvironment(nil))
	if got != "" {
		t.Fatalf("resolveConfigPath = %q, want no configuration file", got)
	}
}

// TestResolveConfigPathIgnoresAnEmptyEnvironmentValue covers an environment
// variable that is set to nothing: it means "no configuration file", the same
// as an unset variable.
func TestResolveConfigPathIgnoresAnEmptyEnvironmentValue(t *testing.T) {
	lookup := lookupEnvironment(map[string]string{ConfigPathEnvironment: "  "})
	got := resolveConfigPath("", lookup)
	if got != "" {
		t.Fatalf("resolveConfigPath = %q, want no configuration file", got)
	}
}

// TestConfigPathEnvironmentIsKnown checks that the configuration file entry
// is not reported as an unknown variable: it is known to the program even
// though it configures no configuration key.
func TestConfigPathEnvironmentIsKnown(t *testing.T) {
	_, err := load("", lookupEnvironment(map[string]string{
		ConfigPathEnvironment: "/etc/torrentfs/torrentfs.toml",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !isEnvironmentException(ConfigPathEnvironment) {
		t.Fatal("the configuration file entry is not a registered exception")
	}
}
