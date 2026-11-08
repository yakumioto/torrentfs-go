// Package configctl presents the configuration to the configuration page.
//
// The controller owns the configuration file path, the effective
// configuration the process started with, and the source of every value. It
// renders the read-only view the HTTP API serves: the schema metadata, the
// value the file layer holds, the effective value, the source, and the reason
// a key cannot be edited.
//
// The save path, the hot apply and the external reload belong to the follow-up
// tickets. The view already carries the zero status of both operations and a
// stable structure, so those tickets extend the controller instead of the
// wire contract.
package configctl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// Stable machine-readable reasons a configuration key, or the whole page,
// cannot be edited. The page branches on these codes; the messages stay
// display-only.
const (
	// ReadOnlyEnvironmentOverride marks a key the environment overrides.
	// The environment is fixed for the life of the process, so saving the
	// file could not change the effective value.
	ReadOnlyEnvironmentOverride = "environment_override"
	// ReadOnlySecret marks a secret key such as the password. Its value is
	// never disclosed and it is never edited from the page.
	ReadOnlySecret = "secret"
	// ReadOnlyNotConfigured marks a deployment that started without a
	// configuration file. There is no file to write a change into.
	ReadOnlyNotConfigured = "not_configured"
	// ReadOnlyFileNotWritable marks a configuration file or its directory
	// that denies writing, for example a read-only mount.
	ReadOnlyFileNotWritable = "file_not_writable"
)

// Controller reads the configuration as the configuration page sees it.
type Controller struct {
	path    string
	cfg     config.Config
	sources config.Sources
}

// New builds a controller for the configuration file at path.
//
// cfg and sources are the effective configuration the process loaded at
// start; the controller does not reload the process environment. An empty
// path is a deployment that started without a configuration file: the page is
// read-only and shows the built-in defaults.
func New(path string, cfg config.Config, sources config.Sources) *Controller {
	return &Controller{path: path, cfg: cfg, sources: sources}
}

// View renders the read-only configuration view.
func (c *Controller) View() (View, error) {
	reason, exists, err := c.readOnlyReason()
	if err != nil {
		return View{}, err
	}
	fileConfig := config.Default()
	if c.path != "" && exists {
		fileConfig, err = config.LoadWithEnvironment(c.path, emptyEnvironment{})
		if err != nil {
			return View{}, fmt.Errorf("configctl: load the file configuration: %w", err)
		}
	}

	defaults := config.Default()
	keys := make([]Key, 0, len(config.Schema()))
	for _, schemaKey := range config.Schema() {
		fileValue, _ := config.ValueText(fileConfig, schemaKey.Path)
		effectiveValue, _ := config.ValueText(c.cfg, schemaKey.Path)
		defaultValue, _ := config.ValueText(defaults, schemaKey.Path)
		readOnly := ""
		switch {
		case schemaKey.Secret:
			// A secret is never disclosed, not even its file value.
			readOnly = ReadOnlySecret
			fileValue = ""
			effectiveValue = ""
		case c.sources.SourceOf(schemaKey.Path) == config.SourceEnvironment:
			readOnly = ReadOnlyEnvironmentOverride
		}
		keys = append(keys, Key{
			Path:           schemaKey.Path,
			Section:        sectionOf(schemaKey.Path),
			Kind:           schemaKey.Kind,
			Values:         append([]string{}, schemaKey.Values...),
			Default:        defaultValue,
			FileValue:      fileValue,
			EffectiveValue: effectiveValue,
			Source:         string(c.sources.SourceOf(schemaKey.Path)),
			Environment:    schemaKey.Environment,
			Overridable:    schemaKey.Overridable,
			Secret:         schemaKey.Secret,
			Component:      schemaKey.Component,
			HotApply:       schemaKey.HotApply,
			Documentation:  schemaKey.Documentation,
			ReadOnlyReason: readOnly,
		})
	}

	return View{
		ConfigFile:     c.path,
		ReadOnlyReason: reason,
		Keys:           keys,
		LastSave:       emptyStatus(),
		LastReload:     emptyStatus(),
	}, nil
}

// readOnlyReason reports why the configuration page is read-only, and whether
// the configuration file exists.
//
// The check reads the declared permission bits instead of probing a write:
// the process may run as root, where opening a read-only file for writing
// succeeds, and a probe would change the file it is asking about. A file
// without a write bit is therefore read-only even for root. The atomic
// write-back creates a temporary file beside the configuration file, so the
// directory needs a write bit as well.
func (c *Controller) readOnlyReason() (string, bool, error) {
	if c.path == "" {
		return ReadOnlyNotConfigured, false, nil
	}
	info, err := os.Stat(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return ReadOnlyNotConfigured, false, nil
		}
		return "", false, fmt.Errorf("configctl: inspect the configuration file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return ReadOnlyFileNotWritable, true, nil
	}
	if info.Mode().Perm()&0o222 == 0 {
		return ReadOnlyFileNotWritable, true, nil
	}
	directory, err := os.Stat(filepath.Dir(c.path))
	if err != nil {
		return "", false, fmt.Errorf("configctl: inspect the configuration directory: %w", err)
	}
	if !directory.IsDir() || directory.Mode().Perm()&0o222 == 0 {
		return ReadOnlyFileNotWritable, true, nil
	}
	return "", true, nil
}

// sectionOf returns the configuration section of a key: everything before its
// last dot. A top-level key belongs to the root section.
func sectionOf(path string) string {
	index := strings.LastIndex(path, ".")
	if index < 0 {
		return ""
	}
	return path[:index]
}

// emptyStatus is the zero status of an operation the read-only view has not
// run yet.
func emptyStatus() Status {
	return Status{Keys: []string{}}
}

// emptyEnvironment is an environment with no variable set.
//
// The file configuration is the built-in defaults overlaid with the file, so
// the view loads it through the same loader with an empty environment instead
// of repeating the merge.
type emptyEnvironment struct{}

// Lookup reports that no environment variable is set.
func (emptyEnvironment) Lookup(string) (string, bool) { return "", false }

// Names reports no environment variable.
func (emptyEnvironment) Names() []string { return nil }

// View is the read-only configuration the configuration page renders.
type View struct {
	// ConfigFile is the configuration file the process loaded at start.
	// Empty means the deployment started without one.
	ConfigFile string `json:"config_file"`
	// ReadOnlyReason explains why the whole page is read-only: not_configured
	// or file_not_writable. Empty means a save would be possible.
	ReadOnlyReason string `json:"read_only_reason"`
	// Keys lists every configuration key in schema order.
	Keys []Key `json:"keys"`
	// LastSave is the status of the most recent save. The read-only view
	// reports the zero status until the save path exists.
	LastSave Status `json:"last_save"`
	// LastReload is the status of the most recent external reload. The
	// read-only view reports the zero status until the reload path exists.
	LastReload Status `json:"last_reload"`
}

// Key is one configuration key of the view.
type Key struct {
	// Path is the dot path of the key.
	Path string `json:"path"`
	// Section is the configuration section that groups the key.
	Section string `json:"section"`
	// Kind names the type of the value.
	Kind string `json:"kind"`
	// Values lists the accepted values of an enumerated key.
	Values []string `json:"values"`
	// Default is the built-in default value, rendered for a human.
	Default string `json:"default"`
	// FileValue is the value of the file layer, before the environment is
	// merged. A secret reports an empty value.
	FileValue string `json:"file_value"`
	// EffectiveValue is the value the process uses. A secret reports an
	// empty value.
	EffectiveValue string `json:"effective_value"`
	// Source names the input the effective value came from: default, file, or
	// environment.
	Source string `json:"source"`
	// Environment is the environment variable the path derives.
	Environment string `json:"environment"`
	// Overridable reports whether the environment may override the key.
	Overridable bool `json:"overridable"`
	// Secret reports whether the value must not be disclosed.
	Secret bool `json:"secret"`
	// Component names the process component that owns the key.
	Component string `json:"component"`
	// HotApply reports whether a new value reaches the running process
	// without a restart.
	HotApply bool `json:"hot_apply"`
	// Documentation is the documentation of the key.
	Documentation string `json:"documentation"`
	// ReadOnlyReason explains why the key cannot be edited, or is empty when
	// it could be edited once the save path exists.
	ReadOnlyReason string `json:"read_only_reason"`
}

// Status is the outcome of a save or a reload.
//
// The read-only view reports the zero value; the save and reload tickets fill
// the timestamp, the changed keys, and the failure reason.
type Status struct {
	// At is when the operation happened, or nil when it never ran.
	At *time.Time `json:"at"`
	// OK reports whether the operation succeeded.
	OK bool `json:"ok"`
	// Error is the failure reason, empty on success or when the operation
	// never ran.
	Error string `json:"error"`
	// Keys lists the configuration keys the operation changed. It never
	// holds a secret value.
	Keys []string `json:"keys"`
}
