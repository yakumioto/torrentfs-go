// Package configctl presents the configuration to the configuration page.
//
// The controller owns the configuration file path, the effective
// configuration the process started with, and the source of every value. It
// renders the read-only view the HTTP API serves: the schema metadata, the
// value the file layer holds, the effective value, the source, and the reason
// a key cannot be edited.
//
// A save validates a sparse patch, writes the configuration file back
// atomically, and hot-applies the changed keys. The external reload polls the
// file digest and runs the same load, validation and apply path, so an edit
// made outside the process reaches the running components without a restart.
// Both operations report their outcome in the view; a rejected external edit
// keeps the last known good configuration in force and in the view.
package configctl

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/configwrite"
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
//
// The controller guards its state with a reader/writer lock: a save replaces
// the effective configuration, the sources and the last-save status while a
// view may be serving them, and the page polls the view while it saves.
type Controller struct {
	mu      sync.RWMutex
	path    string
	cfg     config.Config
	sources config.Sources
	logger  *slog.Logger

	appliers      []registration
	applierByPath map[string]int

	// fileConfig is the file configuration of the most recent load or save;
	// baseline is the digest of the bytes it came from. The view reads the
	// cached pair instead of the file, so an external edit that made the file
	// invalid keeps the last known good values visible. observed is the digest
	// of the most recent bytes the controller looked at, including bytes it
	// rejected: an unchanged invalid file is one failure, not one per poll.
	fileConfig config.Config
	baseline   string
	observed   string
	fileError  string

	// reloadInterval is the poll interval of Watch. newTicker produces its
	// tick channel; the tests inject one to drive the poll without sleeping.
	reloadInterval time.Duration
	newTicker      tickerFactory

	lastSave   Status
	lastReload Status

	// writeBackFile is the atomic write-back of the configuration file. The
	// tests inject a failure at this system boundary; production leaves the
	// configuration package writer.
	writeBackFile func(path string, document []byte) (published bool, err error)
}

// New builds a controller for the configuration file at path.
//
// cfg and sources are the effective configuration the process loaded at
// start; the controller does not reload the process environment. An empty
// path is a deployment that started without a configuration file: the page is
// read-only and shows the built-in defaults.
func New(path string, cfg config.Config, sources config.Sources, options ...Option) *Controller {
	controller := &Controller{
		path:           path,
		cfg:            cfg,
		sources:        sources,
		logger:         slog.New(slog.DiscardHandler),
		applierByPath:  map[string]int{},
		writeBackFile:  config.WriteBackFile,
		reloadInterval: DefaultReloadInterval,
		newTicker:      productionTicker,
	}
	for _, option := range options {
		if option != nil {
			option(controller)
		}
	}
	controller.loadInitialFileState()
	return controller
}

// loadInitialFileState caches the file configuration the process started
// with.
//
// The view and the reload path read the file layer from this cache, so a later
// external edit that makes the file invalid cannot take the view down: the
// page keeps showing the last configuration that loaded. The digest of the
// initial bytes seeds the poll, so the first check after a start does not
// report the file the process just loaded as an external change.
func (c *Controller) loadInitialFileState() {
	c.fileConfig = config.Default()
	if c.path == "" {
		return
	}
	content, err := os.ReadFile(c.path)
	if err != nil {
		c.fileError = fmt.Sprintf("configctl: read the configuration file: %v", err)
		c.observed = missingFileDigest
		return
	}
	digest := configurationBaseline(content)
	c.observed = digest
	file, err := configwrite.Load(content, c.path)
	if err != nil {
		c.fileError = fmt.Sprintf("configctl: load the file configuration: %v", err)
		return
	}
	c.fileConfig = file.Config
	c.baseline = digest
}

// Option configures a Controller.
type Option func(*Controller)

// WithLogger routes the configuration audit to l.
//
// The audit names the configuration keys a save changed and never their
// values, so a secret in the configuration file stays out of the log.
func WithLogger(l *slog.Logger) Option {
	return func(controller *Controller) {
		if l != nil {
			controller.logger = l
		}
	}
}

// View renders the read-only configuration view.
func (c *Controller) View() (View, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	reason, _, err := c.readOnlyReason()
	if err != nil {
		return View{}, err
	}
	// The file values come from the last configuration that loaded, not from
	// a fresh read: an external edit that made the file invalid must degrade
	// the page to the last known good values instead of failing the request.
	fileConfig := c.fileConfig
	baseline := c.baseline

	defaults := config.Default()
	keys := make([]Key, 0, len(config.Schema()))
	for _, schemaKey := range config.Schema() {
		fileValue, _ := config.ValueText(fileConfig, schemaKey.Path)
		effectiveValue, _ := config.ValueText(c.cfg, schemaKey.Path)
		defaultValue, _ := config.ValueText(defaults, schemaKey.Path)
		// A schema key can be hot-applied only when a component registered an
		// applier for it. The badge the page renders must not promise an
		// effect the running process has no way to receive.
		hotApply := schemaKey.HotApply
		if _, registered := c.applierByPath[schemaKey.Path]; !registered {
			hotApply = false
		}
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
			HotApply:       hotApply,
			Documentation:  schemaKey.Documentation,
			ReadOnlyReason: readOnly,
		})
	}

	return View{
		ConfigFile:     c.path,
		Baseline:       baseline,
		ReadOnlyReason: reason,
		FileError:      c.fileError,
		Keys:           keys,
		LastSave:       normalizeStatus(c.lastSave),
		LastReload:     normalizeStatus(c.lastReload),
	}, nil
}

// configurationBaseline returns the digest that tells the save path whether
// the file is still the one the page rendered. The empty digest means there is
// no file to edit.
func configurationBaseline(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// readOnlyReason reports why the configuration page is read-only, and whether
// the configuration file exists.
//
// The check lives in the configuration package, next to the atomic write-back
// it describes, so the page and the startup migration agree on which file can
// be replaced.
func (c *Controller) readOnlyReason() (string, bool, error) {
	if c.path == "" {
		return ReadOnlyNotConfigured, false, nil
	}
	exists, writable, err := config.WriteBackAvailable(c.path)
	if err != nil {
		return "", false, fmt.Errorf("configctl: %w", err)
	}
	switch {
	case !exists:
		return ReadOnlyNotConfigured, false, nil
	case !writable:
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

// normalizeStatus keeps the keys of a reported status an empty list instead of
// null, so a client always reads the same shape.
func normalizeStatus(status Status) Status {
	if status.Keys == nil {
		status.Keys = []string{}
	}
	return status
}

// View is the read-only configuration the configuration page renders.
type View struct {
	// ConfigFile is the configuration file the process loaded at start.
	// Empty means the deployment started without one.
	ConfigFile string `json:"config_file"`
	// Baseline is a digest of the configuration file bytes the page was
	// rendered from. A save sends it back; a file another writer changed has
	// a different digest, so the save is refused instead of overwriting the
	// change. Empty means there is no configuration file.
	Baseline string `json:"baseline"`
	// ReadOnlyReason explains why the whole page is read-only: not_configured
	// or file_not_writable. Empty means a save would be possible.
	ReadOnlyReason string `json:"read_only_reason"`
	// FileError reports why the configuration file on disk cannot be used
	// right now, for example because an external edit made it invalid. The
	// values of the view stay those of the last successful load, so a broken
	// file degrades the page instead of failing it. Empty means the file
	// still matches the values shown.
	FileError string `json:"file_error"`
	// Keys lists every configuration key in schema order.
	Keys []Key `json:"keys"`
	// LastSave is the status of the most recent save, and the zero status
	// before the first save.
	LastSave Status `json:"last_save"`
	// LastReload is the status of the most recent external reload. The zero
	// status means no external change was detected since the start.
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
// The zero value means the operation has not run since the start: no
// timestamp, no keys, no failure. A failed operation reports OK=false with the
// reason in Error; the keys of a failed reload are empty because no value
// changed.
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
