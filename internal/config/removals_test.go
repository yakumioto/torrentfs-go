package config

import (
	"errors"
	"strings"
	"testing"
)

// TestRemovedNamesAreRejectedWithTheirReplacement covers every entry of the
// removal table: the loader must reject the old name and name the
// replacement, so a user can migrate without reading the source code.
func TestRemovedNamesAreRejectedWithTheirReplacement(t *testing.T) {
	for _, removed := range RemovedNames() {
		t.Run(removed.Kind+" "+removed.Old, func(t *testing.T) {
			var err error
			switch removed.Kind {
			case removedEnvironment:
				_, err = load("", lookupEnvironment(map[string]string{removed.Old: "value"}))
			case removedCommandFlag:
				err = RemovedFlagError([]string{removed.Old, "/torrents"})
			default:
				_, err = load(writeConfigFile(t, removedTOMLDocument(removed.Old)), lookupEnvironment(nil))
			}
			if err == nil {
				t.Fatalf("removed name %q was accepted", removed.Old)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v, want ErrInvalid", err)
			}
			message := err.Error()
			if !strings.Contains(message, removed.Old) {
				t.Fatalf("error = %q, want it to name %q", message, removed.Old)
			}
			for _, replacement := range removed.New {
				if !strings.Contains(message, replacement) {
					t.Fatalf("error = %q, want it to name the replacement %q", message, replacement)
				}
			}
		})
	}
}

// TestRemovedNamesAreDistinct guards the table against duplicate entries,
// which would make the error message mention one name twice.
func TestRemovedNamesAreDistinct(t *testing.T) {
	seen := make(map[string]bool)
	for _, removed := range RemovedNames() {
		key := removed.Kind + " " + removed.Old
		if seen[key] {
			t.Fatalf("removed name %q is registered twice", key)
		}
		seen[key] = true
		if len(removed.New) == 0 {
			t.Fatalf("removed name %q has no replacement", key)
		}
	}
}

// TestRemovedTOMLNamesReportTheMostSpecificKey checks that a document that
// sets a removed key is reported with that key and not with the removed
// section it belongs to.
func TestRemovedTOMLNamesReportTheMostSpecificKey(t *testing.T) {
	_, err := load(writeConfigFile(t, "[mount]\nread_timeout = \"3s\"\n"), lookupEnvironment(nil))
	if err == nil {
		t.Fatal("the removed [mount] section was accepted")
	}
	message := err.Error()
	if !strings.Contains(message, "mount.read_timeout") {
		t.Fatalf("error = %q, want the specific key mount.read_timeout", message)
	}
	if !strings.Contains(message, "filesystem.read_timeout") {
		t.Fatalf("error = %q, want the replacement filesystem.read_timeout", message)
	}
}

// TestRemovedEnvironmentNamesDoNotAffectOtherValues checks that a removed
// environment variable is rejected on its own, without disturbing the
// configuration that the user did set.
func TestRemovedEnvironmentNamesDoNotAffectOtherValues(t *testing.T) {
	_, err := load("", lookupEnvironment(map[string]string{
		"TORRENTFS_CACHE_CAPACITY":    "123456B",
		"TORRENTFS_MOUNT_ALLOW_OTHER": "true",
	}))
	if err == nil {
		t.Fatal("the removed TORRENTFS_MOUNT_ALLOW_OTHER was accepted")
	}
	message := err.Error()
	if !strings.Contains(message, "TORRENTFS_MOUNT_ALLOW_OTHER") {
		t.Fatalf("error = %q, want the removed variable name", message)
	}
	if !strings.Contains(message, "TORRENTFS_FUSE_ALLOW_OTHER") {
		t.Fatalf("error = %q, want the replacement name", message)
	}
}

// TestRemovedFlagErrorIgnoresUnrelatedArguments checks that the command-line
// check only rejects the flags that were removed.
func TestRemovedFlagErrorIgnoresUnrelatedArguments(t *testing.T) {
	for _, args := range [][]string{
		{"-config", "/etc/torrentfs/torrentfs.toml", "/torrents"},
		{"--config=/etc/torrentfs/torrentfs.toml", "/torrents"},
		{"/torrents"},
	} {
		if err := RemovedFlagError(args); err != nil {
			t.Fatalf("RemovedFlagError(%v) = %v, want no error", args, err)
		}
	}
}

// TestRemovedFlagErrorAcceptsBothFlagForms checks that a removed flag is
// recognized in the single-dash, double-dash, and "=" forms.
func TestRemovedFlagErrorAcceptsBothFlagForms(t *testing.T) {
	for _, arg := range []string{"-mountpoint", "--mountpoint", "-mountpoint=/srv/mnt", "--mountpoint=/srv/mnt"} {
		err := RemovedFlagError([]string{arg, "/torrents"})
		if err == nil {
			t.Fatalf("RemovedFlagError(%q) = nil, want an error", arg)
		}
		if !strings.Contains(err.Error(), "fuse.mountpoint") {
			t.Fatalf("error = %q, want the replacement fuse.mountpoint", err)
		}
	}
}

// TestEnvironmentSeamEnumeratesAndLooksUp checks the environment seam
// contract: a caller can ask for one variable by name and can enumerate every
// variable that is set. Unknown-variable detection builds on both.
func TestEnvironmentSeamEnumeratesAndLooksUp(t *testing.T) {
	values := map[string]string{
		"TORRENTFS_CACHE_CAPACITY": "1GiB",
		"TF_TEST_ONLY":             "not a configuration",
	}
	env := lookupEnvironment(values)

	for name, want := range values {
		got, ok := env.Lookup(name)
		if !ok || got != want {
			t.Fatalf("Lookup(%q) = (%q, %v), want (%q, true)", name, got, ok, want)
		}
	}
	if _, ok := env.Lookup("TORRENTFS_MISSING"); ok {
		t.Fatal("Lookup reported an unset variable as set")
	}

	names := env.Names()
	if len(names) != len(values) {
		t.Fatalf("Names() = %v, want %d names", names, len(values))
	}
	for _, name := range names {
		if _, ok := values[name]; !ok {
			t.Fatalf("Names() listed %q, which is not set", name)
		}
	}
}

// TestProcessEnvironmentEnumerates checks that the real environment source
// enumerates the variables that are set.
func TestProcessEnvironmentEnumerates(t *testing.T) {
	t.Setenv("TF_TEST_ONLY", "value")
	env := osEnvironment{}
	if _, ok := env.Lookup("TF_TEST_ONLY"); !ok {
		t.Fatal("Lookup did not find a variable that is set")
	}
	for _, name := range env.Names() {
		if name == "TF_TEST_ONLY" {
			return
		}
	}
	t.Fatal("Names did not list a variable that is set")
}

func removedTOMLDocument(key string) string {
	switch key {
	case "mount":
		// A removed section without one of its keys: the section itself is
		// what the user has to migrate away from.
		return "[mount]\nunused = true\n"
	case "mount.read_timeout":
		return "[mount]\nread_timeout = \"3s\"\n"
	case "mount.allow_other":
		return "[mount]\nallow_other = true\n"
	default:
		return "[unknown]\nvalue = true\n"
	}
}
