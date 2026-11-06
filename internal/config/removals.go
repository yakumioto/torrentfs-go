package config

import (
	"fmt"
	"strings"
)

// The configuration namespaces a removed name can come from. The kind decides
// how a name is matched and how the migration error reads.
const (
	removedTOMLKey     = "TOML key"
	removedEnvironment = "environment variable"
	removedCommandFlag = "command-line flag"
)

// RemovedName describes one configuration name that no longer exists together
// with the names that replace it.
type RemovedName struct {
	// Kind names the configuration namespace the removed name belongs to.
	Kind string
	// Old is the removed name exactly as a user writes it.
	Old string
	// New lists the replacements that may be used instead.
	New []string
}

// RemovedError reports configuration names that no longer exist. It names the
// source of each name and its replacement, so a user can migrate without
// reading the source code.
type RemovedError struct {
	Names []RemovedName
}

func (e *RemovedError) Error() string {
	messages := make([]string, 0, len(e.Names))
	for _, name := range e.Names {
		messages = append(messages, name.migration())
	}
	return strings.Join(messages, "; ")
}

// Is reports that a removed name is an invalid configuration.
func (e *RemovedError) Is(target error) bool { return target == ErrInvalid }

// migration renders the guidance for one removed name.
func (n RemovedName) migration() string {
	replacements := make([]string, 0, len(n.New))
	for _, name := range n.New {
		replacements = append(replacements, fmt.Sprintf("%q", name))
	}
	switch n.Kind {
	case removedEnvironment:
		return fmt.Sprintf("removed environment variable %q; use the environment variable %s instead",
			n.Old, strings.Join(replacements, " or "))
	case removedCommandFlag:
		return fmt.Sprintf("removed command-line flag %q; use the configuration key %s instead",
			n.Old, strings.Join(replacements, " or "))
	default:
		return fmt.Sprintf("removed TOML key %q; use the configuration key %s instead",
			n.Old, strings.Join(replacements, " or "))
	}
}

// removedNames lists every configuration name that has been removed. Startup
// errors and the README migration table are both derived from this table, so a
// name cannot disappear from the code while it is still documented.
//
// TOML keys are matched most specific first: a document that sets
// "mount.read_timeout" is reported with that exact key instead of with the
// removed "[mount]" section.
var removedNames = []RemovedName{
	{Kind: removedTOMLKey, Old: "mount.read_timeout", New: []string{"filesystem.read_timeout"}},
	{Kind: removedTOMLKey, Old: "mount.allow_other", New: []string{"fuse.allow_other"}},
	{Kind: removedTOMLKey, Old: "mount", New: []string{"filesystem.read_timeout", "fuse.allow_other"}},
	{Kind: removedTOMLKey, Old: "filesystem.backend", New: []string{"backend"}},
	{Kind: removedTOMLKey, Old: "smb.strm_enabled", New: []string{"smb.strm"}},
	{Kind: removedTOMLKey, Old: "http.auth.username", New: []string{"username"}},
	{Kind: removedTOMLKey, Old: "http.auth.password_hash", New: []string{"password"}},
	{Kind: removedTOMLKey, Old: "http.auth.password_hash_file", New: []string{"password"}},
	{Kind: removedTOMLKey, Old: "smb.username", New: []string{"username"}},
	{Kind: removedTOMLKey, Old: "smb.password_file", New: []string{"password"}},
	{Kind: removedCommandFlag, Old: "-mountpoint", New: []string{"fuse.mountpoint"}},
	{Kind: removedCommandFlag, Old: "-strm-mountpoint", New: []string{"fuse.strm_mountpoint"}},
	{Kind: removedEnvironment, Old: "TORRENTFS_MOUNT_READ_TIMEOUT", New: []string{"TORRENTFS_FILESYSTEM_READ_TIMEOUT"}},
	{Kind: removedEnvironment, Old: "TORRENTFS_MOUNT_ALLOW_OTHER", New: []string{"TORRENTFS_FUSE_ALLOW_OTHER"}},
	{Kind: removedEnvironment, Old: "TORRENTFS_SMB_ENABLED", New: []string{"TORRENTFS_BACKEND"}},
	{Kind: removedEnvironment, Old: "TORRENTFS_STRM_ENABLED", New: []string{"TORRENTFS_SMB_STRM"}},
	{Kind: removedEnvironment, Old: "TORRENTFS_FILESYSTEM_BACKEND", New: []string{"TORRENTFS_BACKEND"}},
	{Kind: removedEnvironment, Old: "TORRENTFS_SMB_STRM_ENABLED", New: []string{"TORRENTFS_SMB_STRM"}},
	{Kind: removedEnvironment, Old: "TORRENTFS_SMB_USERNAME", New: []string{"TORRENTFS_USERNAME"}},
	{Kind: removedEnvironment, Old: "TORRENTFS_SMB_PASSWORD_FILE", New: []string{"TORRENTFS_PASSWORD"}},
}

// RemovedNames returns every removed configuration name with its replacement.
// The result is a copy: callers cannot modify the table itself.
func RemovedNames() []RemovedName {
	names := make([]RemovedName, len(removedNames))
	for i, name := range removedNames {
		names[i] = name
		names[i].New = append([]string(nil), name.New...)
	}
	return names
}

// removedTOMLNames reports the removed TOML names a document sets. The most
// specific match wins, so a document that sets "mount.read_timeout" is
// reported with that key and not with the removed "[mount]" section.
func removedTOMLNames(document map[string]any) []RemovedName {
	var matched []RemovedName
	for _, name := range removedNames {
		if name.Kind != removedTOMLKey {
			continue
		}
		if documentUses(document, name.Old) {
			matched = append(matched, name)
		}
	}
	// A matched ancestor adds nothing: "mount" is already covered by the more
	// specific "mount.read_timeout" that was reported.
	var specific []RemovedName
	for _, name := range matched {
		if coveredByMoreSpecific(name, matched) {
			continue
		}
		specific = append(specific, name)
	}
	return specific
}

// coveredByMoreSpecific reports whether another match names a key below the
// given one, which makes the given one redundant.
func coveredByMoreSpecific(name RemovedName, matched []RemovedName) bool {
	for _, other := range matched {
		if other.Old == name.Old {
			continue
		}
		if strings.HasPrefix(other.Old, name.Old+".") {
			return true
		}
	}
	return false
}

// removedEnvironmentNames reports the removed environment variables the
// environment sets.
func removedEnvironmentNames(lookup envLookup) []RemovedName {
	var matched []RemovedName
	for _, name := range removedNames {
		if name.Kind != removedEnvironment {
			continue
		}
		if _, ok := lookup.Lookup(name.Old); ok {
			matched = append(matched, name)
		}
	}
	return matched
}

// RemovedFlagError reports whether the command-line arguments use a flag that
// no longer exists, naming the configuration key that replaces it.
func RemovedFlagError(args []string) error {
	var matched []RemovedName
	for _, arg := range args {
		name := flagName(arg)
		if name == "" {
			continue
		}
		for _, removed := range removedNames {
			if removed.Kind == removedCommandFlag && removed.Old == name {
				matched = append(matched, removed)
			}
		}
	}
	if len(matched) == 0 {
		return nil
	}
	return &RemovedError{Names: matched}
}

// flagName extracts a normalized flag name from one command-line argument.
// Both "-flag" and "--flag=value" forms map onto "-flag".
func flagName(arg string) string {
	if !strings.HasPrefix(arg, "-") {
		return ""
	}
	name := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	if index := strings.Index(name, "="); index >= 0 {
		name = name[:index]
	}
	if name == "" {
		return ""
	}
	return "-" + name
}

// documentUses reports whether a decoded TOML document sets the given
// dot-separated key path.
func documentUses(document map[string]any, path string) bool {
	segments := strings.Split(path, ".")
	value, ok := document[segments[0]]
	if !ok {
		return false
	}
	for index, segment := range segments[1:] {
		table, ok := value.(map[string]any)
		if !ok {
			// A scalar value ends the path: the key itself exists.
			return index == len(segments)-2
		}
		value, ok = table[segment]
		if !ok {
			return false
		}
	}
	return true
}
