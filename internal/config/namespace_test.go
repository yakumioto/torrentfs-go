package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestUnknownEnvironmentVariableIsRejected covers the strictness of the
// configuration namespace: a variable that no configuration key derives is
// rejected by name, so a misspelling cannot start the process with a default
// value the user never asked for.
func TestUnknownEnvironmentVariableIsRejected(t *testing.T) {
	_, err := load("", lookupEnvironment(map[string]string{
		"TORRENTFS_HTTP_LISEN_ADDR": "0.0.0.0:8080",
	}))
	if err == nil {
		t.Fatal("load accepted the unknown environment variable")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("load error = %v, want ErrInvalid", err)
	}
	message := err.Error()
	for _, want := range []string{"TORRENTFS_HTTP_LISEN_ADDR", "not a known configuration item"} {
		if !strings.Contains(message, want) {
			t.Fatalf("load error = %q, want it to name %q", message, want)
		}
	}
}

// TestUnknownEnvironmentVariablesAreReportedTogether covers the aggregation
// contract: a user who misspelled two names learns about both at once.
func TestUnknownEnvironmentVariablesAreReportedTogether(t *testing.T) {
	_, err := load("", lookupEnvironment(map[string]string{
		"TORRENTFS_HTTP_LISEN_ADDR": "0.0.0.0:8080",
		"TORRENTFS_CACHE_CAPACIY":   "32MiB",
	}))
	if err == nil {
		t.Fatal("load accepted the unknown environment variables")
	}
	for _, want := range []string{"TORRENTFS_HTTP_LISEN_ADDR", "TORRENTFS_CACHE_CAPACIY"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("load error = %q, want it to name %q", err, want)
		}
	}
}

// TestKnownEnvironmentVariablesAreAccepted checks that the strictness does
// not reject a variable the schema derives.
func TestKnownEnvironmentVariablesAreAccepted(t *testing.T) {
	for _, key := range Schema() {
		if !key.Overridable {
			continue
		}
		if isEnvironmentException(key.Environment) {
			t.Fatalf("the schema derives %q, which the exception table also covers", key.Environment)
		}
		_, err := load("", lookupEnvironment(map[string]string{key.Environment: "value"}))
		if err != nil && strings.Contains(err.Error(), "not a known configuration item") {
			t.Fatalf("the schema-derived variable %q was reported as unknown: %v", key.Environment, err)
		}
	}
}

// TestEnvironmentOutsideTheNamespaceIsIgnored checks that the strictness
// applies to the configuration namespace only: a variable of another
// namespace is not configuration and is left alone.
func TestEnvironmentOutsideTheNamespaceIsIgnored(t *testing.T) {
	got, err := load("", lookupEnvironment(map[string]string{
		"TF_FUSE_REQUIRED": "1",
		"TF_SMOKE_IMAGE":   "example:latest",
		"PATH":             "/usr/bin",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(got, Default()) {
		t.Fatalf("config changed after variables outside the namespace: %+v", got)
	}
}

// TestRemovedEnvironmentVariableIsNotReportedAsUnknown checks that a removed
// name keeps its own report: the removal table names the replacement, which
// helps the user more than the unknown-variable rule.
func TestRemovedEnvironmentVariableIsNotReportedAsUnknown(t *testing.T) {
	for _, removed := range RemovedNames() {
		if removed.Kind != removedEnvironment {
			continue
		}
		_, err := load("", lookupEnvironment(map[string]string{removed.Old: "value"}))
		if err == nil {
			t.Fatalf("load accepted the removed environment variable %q", removed.Old)
		}
		if strings.Contains(err.Error(), "not a known configuration item") {
			t.Fatalf("load error = %q, want the removal report for %q", err, removed.Old)
		}
	}
}

// TestEnvironmentExceptionsCoverTheirVariables checks that every registered
// exception names a variable that no configuration key derives. An exception
// that shadows a configuration key would hide a real setting.
func TestEnvironmentExceptionsCoverTheirVariables(t *testing.T) {
	for _, exception := range environmentExceptions {
		if exception.Name == "" && exception.Prefix == "" {
			t.Fatal("an environment exception names neither a variable nor a prefix")
		}
		if exception.Name != "" && exception.Prefix != "" {
			t.Fatalf("the environment exception %q names both a variable and a prefix", exception.Name)
		}
		if exception.Name != "" {
			for _, item := range configSchema {
				if item.environment == exception.Name {
					t.Fatalf("the environment exception %q shadows a configuration key", exception.Name)
				}
			}
		}
	}
}

// TestEnvironmentExceptionsDoNotCoverTheConfigurationNamespace checks that an
// exception never covers the whole configuration namespace, which would turn
// the unknown-variable rule off.
func TestEnvironmentExceptionsDoNotCoverTheConfigurationNamespace(t *testing.T) {
	for _, exception := range environmentExceptions {
		if exception.Prefix == environmentPrefix {
			t.Fatalf("the environment exception %q covers the whole configuration namespace", exception.Prefix)
		}
	}
}
