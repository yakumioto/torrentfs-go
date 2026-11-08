package configctl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/configcheck"
)

// Stable machine-readable reasons a save was rejected. The page branches on
// these codes; the messages stay display-only.
const (
	// SaveInvalid marks a patch or a resulting configuration that violates
	// the configuration rules. FieldErrors names every rejected key.
	SaveInvalid = "invalid"
	// SaveConflict marks a configuration file another writer changed after
	// the page was rendered. Nothing is written.
	SaveConflict = "conflict"
	// SaveReadOnly marks a key or a file the page cannot edit: an environment
	// override, a secret, or an unwritable configuration file.
	SaveReadOnly = "read_only"
	// SaveNotConfigured marks a deployment that started without a
	// configuration file.
	SaveNotConfigured = "not_configured"
	// SavePersistenceUnavailable marks a write-back that did not replace the
	// file. The file and the running configuration are unchanged.
	SavePersistenceUnavailable = "persistence_unavailable"
	// SaveDurabilityUnconfirmed marks a write-back that replaced the file but
	// could not confirm the directory sync. The new values are in force, but a
	// restart is not guaranteed to observe them.
	SaveDurabilityUnconfirmed = "durability_unconfirmed"
)

// Applier applies the new values of configuration keys to one component of
// the running process.
//
// A component registers one applier for the keys it can replace while the
// process runs. The controller calls Apply once per component with the whole
// validated configuration, so an applier that relates two keys sees them
// together. A failure moves the keys to restart-required: the file is already
// written, so the next start still adopts the value.
type Applier interface {
	// Keys returns the configuration keys the applier can apply. Every key
	// must be marked hot-applied by the configuration schema.
	Keys() []string
	// Apply applies the values of cfg that belong to the applier.
	Apply(cfg config.Config) error
}

// SaveRequest is a sparse configuration patch and the file baseline it was
// edited from.
type SaveRequest struct {
	// Baseline is the digest the view reported for the configuration file.
	Baseline string
	// Patch maps a configuration key to the text form of its new value. The
	// patch is sparse: a key that is absent keeps the value the file holds.
	Patch map[string]string
}

// SaveResult is the outcome of a save that replaced the configuration file.
type SaveResult struct {
	// Applied lists the changed keys that took effect in the running
	// process, in schema order.
	Applied []string
	// RestartRequired lists the changed keys that are written but take
	// effect at the next start, in schema order.
	RestartRequired []string
	// Persisted reports whether the new file is confirmed to survive a
	// crash. The rename is the commit point; Persisted is false only when
	// the directory sync after it failed.
	Persisted bool
}

// FieldError is one problem of a rejected save.
type FieldError struct {
	// Path is the dot path of the configuration key, or empty for a problem
	// that belongs to no single key.
	Path string `json:"path"`
	// Message is the problem in the words of the start path.
	Message string `json:"message"`
}

// SaveError is a save that did not complete.
type SaveError struct {
	// Code is the stable machine-readable reason. It is one of the Save*
	// constants.
	Code string
	// Err is the underlying problem, or nil when the code carries the whole
	// reason.
	Err error
	// FieldErrors lists the rejected configuration keys.
	FieldErrors []FieldError
	// Applied and RestartRequired report the state of a save that committed
	// without confirmed durability: the values are already in force and must
	// not be retried as if nothing had happened.
	Applied         []string
	RestartRequired []string
	// Persisted reports whether the committed file is confirmed durable.
	Persisted bool
}

// Error renders the rejection.
func (e *SaveError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("configctl: save configuration: %s", e.Code)
	}
	return fmt.Sprintf("configctl: save configuration: %s: %v", e.Code, e.Err)
}

// Unwrap returns the underlying problem so errors.Is reaches it.
func (e *SaveError) Unwrap() error { return e.Err }

// registration is one applier and the keys it owns.
type registration struct {
	applier Applier
	keys    []string
}

// RegisterApplier registers the component applier for its keys.
//
// The controller is built before the HTTP server, because the server needs the
// controller for its routes. An applier that needs a handle the server creates
// is registered after the server is built, which keeps the construction order
// one way: register-appliers never needs the server to build the controller.
//
// A key must be hot-applied by the schema and may belong to one applier only,
// so the page cannot promise an effect two components disagree about.
func (c *Controller) RegisterApplier(applier Applier) error {
	if applier == nil {
		return errors.New("configctl: register a nil applier")
	}
	keys := applier.Keys()
	if len(keys) == 0 {
		return errors.New("configctl: register an applier without configuration keys")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, path := range keys {
		key, ok := schemaKeyFor(path)
		if !ok {
			return fmt.Errorf("configctl: applier owns unknown configuration key %q", path)
		}
		if !key.HotApply {
			return fmt.Errorf("configctl: applier owns restart-only configuration key %q", path)
		}
		if _, exists := c.applierByPath[path]; exists {
			return fmt.Errorf("configctl: configuration key %q already has an applier", path)
		}
	}
	index := len(c.appliers)
	c.appliers = append(c.appliers, registration{applier: applier, keys: append([]string{}, keys...)})
	for _, path := range keys {
		c.applierByPath[path] = index
	}
	return nil
}

// Save validates a sparse patch, writes it back atomically, and hot-applies
// the changed keys.
//
// The save has three stages. Validation loads the rendered document through
// the same loader and process rules as a start, so the page cannot accept a
// configuration a start would reject, and it happens before the final rename,
// so a rejected save never changes the file. The write-back replaces the file
// atomically and keeps a backup. Hot apply runs only after the rename
// committed: a value the file holds but the running process cannot replace is
// reported as restart-required.
//
// A configuration file another writer changed since the view is a conflict:
// the save refuses to overwrite it. A failure before the rename leaves the
// file and the running configuration unchanged; a failure of the directory
// sync after it reports durability_unconfirmed with the values already
// applied.
func (c *Controller) Save(ctx context.Context, request SaveRequest) (SaveResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return SaveResult{}, err
	}

	reason, exists, err := c.readOnlyReason()
	if err != nil {
		return SaveResult{}, err
	}
	switch {
	case c.path == "" || !exists:
		return c.failSaveLocked(SaveNotConfigured,
			errors.New("the process started without a configuration file"), nil)
	case reason != "":
		return c.failSaveLocked(SaveReadOnly,
			errors.New("the configuration file is read-only"), nil)
	}

	content, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return c.failSaveLocked(SaveNotConfigured,
				errors.New("the configuration file disappeared"), nil)
		}
		return SaveResult{}, fmt.Errorf("configctl: read the configuration file: %w", err)
	}
	if request.Baseline != configurationBaseline(content) {
		return c.failSaveLocked(SaveConflict,
			errors.New("the configuration file changed since the page was read"), nil)
	}

	// The patch is sparse, but every key it names must be one the page can
	// edit: an unknown key is a malformed request, a secret or an
	// environment-overridden key could never take effect from the file.
	changed := orderedPatchKeys(request.Patch)
	var invalidFields, readOnlyFields []FieldError
	for _, path := range changed {
		key, ok := schemaKeyFor(path)
		if !ok {
			invalidFields = append(invalidFields, FieldError{Path: path, Message: "unknown configuration key"})
			continue
		}
		if key.Secret {
			readOnlyFields = append(readOnlyFields, FieldError{Path: path, Message: "a secret configuration key cannot be edited"})
			continue
		}
		if c.sources.SourceOf(path) == config.SourceEnvironment {
			readOnlyFields = append(readOnlyFields, FieldError{
				Path:    path,
				Message: fmt.Sprintf("the environment variable %s overrides the configuration file", key.Environment),
			})
		}
	}
	if len(readOnlyFields) > 0 {
		return c.failSaveLocked(SaveReadOnly, errors.New("a key of the patch is read-only"),
			append(invalidFields, readOnlyFields...))
	}
	if len(invalidFields) > 0 {
		return c.failSaveLocked(SaveInvalid, errors.New("the patch names an unknown configuration key"), invalidFields)
	}

	// The write-back renders the file configuration layer: the built-in
	// defaults overlaid with the file, before the environment is merged. The
	// rendered document was read from content, so the candidate is derived
	// from exactly the bytes the baseline covered.
	fileConfig, fileSources, err := config.LoadContentWithSourcesFromEnvironment(content, c.path, emptyEnvironment{})
	if err != nil {
		return c.failSaveLocked(SaveInvalid, err, fieldErrorsOf(err))
	}
	candidate := fileConfig
	var parseErr error
	var parseFields []FieldError
	for _, path := range changed {
		if err := config.SetText(&candidate, path, request.Patch[path]); err != nil {
			parseErr = errors.Join(parseErr, err)
			parseFields = append(parseFields, fieldErrorsOf(err)...)
		}
	}

	document, err := config.WriteBack{File: candidate, Sources: fileSources, Changed: changed}.TOML()
	if err != nil {
		return c.failSaveLocked(SaveInvalid, err, parseFields)
	}
	effective, effectiveSources, checkErr := configcheck.LoadContent(document, c.path)
	if parseErr != nil || checkErr != nil {
		fields := append(parseFields, fieldErrorsOf(checkErr)...)
		// A parse failure leaves the key out of the candidate, so the
		// validation of the other keys still runs and one save reports every
		// problem.
		cause := errors.Join(parseErr, checkErr)
		if cause == nil {
			cause = errors.New("the patch is invalid")
		}
		return c.failSaveLocked(SaveInvalid, cause, fields)
	}

	published, writeErr := c.writeBackFile(c.path, document)
	if !published {
		if writeErr == nil {
			writeErr = errors.New("the write-back did not publish the configuration file")
		}
		return c.failSaveLocked(SavePersistenceUnavailable, writeErr, nil)
	}

	// The rename committed: the new file is what a restart reads, so the
	// write-back is adopted even when the directory sync failed. The running
	// configuration adopts only the keys that took effect; a changed key that
	// waits for a restart keeps the value and source the process still uses,
	// so the view never reports an effect that did not happen.
	applied, restartRequired := c.applyLocked(effective, changed)
	c.cfg, c.sources = runningConfiguration(c.cfg, c.sources, effective, effectiveSources, applied)
	// The controller wrote these bytes itself: adopt them as the file the
	// view shows and as the baseline the external poll compares against, so
	// the next poll neither reloads the save nor logs it again.
	c.fileConfig = candidate
	c.baseline = configurationBaseline(document)
	c.observed = c.baseline
	c.fileError = ""

	result := SaveResult{Applied: applied, RestartRequired: restartRequired, Persisted: writeErr == nil}
	now := time.Now().UTC()
	if writeErr != nil {
		c.lastSave = Status{At: &now, OK: false, Error: writeErr.Error(), Keys: changed}
		c.logger.Error("configuration saved without confirmed durability",
			slog.String("keys", strings.Join(changed, ",")), "err", writeErr)
		return result, &SaveError{
			Code:            SaveDurabilityUnconfirmed,
			Err:             writeErr,
			Applied:         applied,
			RestartRequired: restartRequired,
			Persisted:       false,
		}
	}

	c.lastSave = Status{At: &now, OK: true, Keys: changed}
	// The audit names the configuration keys only: a value, and in particular
	// a secret, never reaches the log.
	c.logger.Info("configuration saved",
		slog.String("keys", strings.Join(changed, ",")),
		slog.String("applied", strings.Join(applied, ",")),
		slog.String("restart_required", strings.Join(restartRequired, ",")))
	return result, nil
}

// failSaveLocked records a failed save and returns its structured error. The
// caller holds the lock.
func (c *Controller) failSaveLocked(code string, cause error, fields []FieldError) (SaveResult, error) {
	message := code
	if cause != nil {
		message = cause.Error()
	}
	c.recordSaveLocked(false, message, nil)
	return SaveResult{}, &SaveError{Code: code, Err: cause, FieldErrors: fields}
}

// recordSaveLocked records the outcome of the most recent save. The caller
// holds the lock.
func (c *Controller) recordSaveLocked(ok bool, message string, keys []string) {
	now := time.Now().UTC()
	if keys == nil {
		keys = []string{}
	}
	c.lastSave = Status{At: &now, OK: ok, Error: message, Keys: keys}
}

// applyLocked hot-applies the changed keys and reports which took effect and
// which wait for a restart. The caller holds the lock.
func (c *Controller) applyLocked(cfg config.Config, changed []string) (applied, restart []string) {
	type applierGroup struct {
		index int
		keys  []string
	}
	var groups []applierGroup
	for _, path := range changed {
		index, ok := c.applierByPath[path]
		if !ok {
			restart = append(restart, path)
			continue
		}
		found := false
		for position := range groups {
			if groups[position].index == index {
				groups[position].keys = append(groups[position].keys, path)
				found = true
				break
			}
		}
		if !found {
			groups = append(groups, applierGroup{index: index, keys: []string{path}})
		}
	}
	for _, group := range groups {
		if err := c.appliers[group.index].applier.Apply(cfg); err != nil {
			// The file is written, so a failed hot apply is a restart-required
			// key instead of a failed save. The audit keeps the failure
			// visible.
			c.logger.Error("configuration hot apply failed",
				slog.String("keys", strings.Join(group.keys, ",")), "err", err)
			restart = append(restart, group.keys...)
			continue
		}
		applied = append(applied, group.keys...)
	}
	return schemaOrder(applied), schemaOrder(restart)
}

// runningConfiguration returns the configuration and sources the process now
// uses: the previous state with the values of the applied keys replaced by the
// new ones.
//
// A key that waits for a restart keeps its previous value even though the file
// already holds the new one, because the running component never received it.
func runningConfiguration(previous config.Config, previousSources config.Sources, effective config.Config, effectiveSources config.Sources, applied []string) (config.Config, config.Sources) {
	running := previous
	if err := config.MergeKeys(&running, effective, applied); err != nil {
		// Every applied key comes from the schema, so a failure here is a
		// programming error; keeping the previous value is the safe result.
		return previous, previousSources
	}
	sources := make(config.Sources, len(previousSources))
	for path, source := range previousSources {
		sources[path] = source
	}
	for _, path := range applied {
		sources[path] = effectiveSources.SourceOf(path)
	}
	return running, sources
}

// schemaKeyFor returns the schema metadata of a configuration key.
func schemaKeyFor(path string) (config.SchemaKey, bool) {
	for _, key := range config.Schema() {
		if key.Path == path {
			return key, true
		}
	}
	return config.SchemaKey{}, false
}

// orderedPatchKeys returns the patch keys in schema order, followed by the
// unknown keys in lexical order so a malformed patch reports deterministically.
func orderedPatchKeys(patch map[string]string) []string {
	known := make([]string, 0, len(patch))
	unknown := make([]string, 0, len(patch))
	inPatch := make(map[string]bool, len(patch))
	for path := range patch {
		inPatch[path] = true
	}
	for _, key := range config.Schema() {
		if inPatch[key.Path] {
			known = append(known, key.Path)
		}
	}
	for path := range patch {
		if _, ok := schemaKeyFor(path); !ok {
			unknown = append(unknown, path)
		}
	}
	sort.Strings(unknown)
	return append(known, unknown...)
}

// schemaOrder sorts configuration keys by their position in the schema.
func schemaOrder(paths []string) []string {
	sorted := append([]string{}, paths...)
	positions := make(map[string]int, len(config.Schema()))
	for index, key := range config.Schema() {
		positions[key.Path] = index
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		left, leftOK := positions[sorted[i]]
		right, rightOK := positions[sorted[j]]
		if leftOK && rightOK {
			return left < right
		}
		return leftOK && !rightOK
	})
	return sorted
}

// fieldErrorsOf converts a configuration problem into a save field problem.
func fieldErrorsOf(err error) []FieldError {
	fields := configcheck.FieldErrors(err)
	converted := make([]FieldError, 0, len(fields))
	for _, field := range fields {
		converted = append(converted, FieldError{Path: field.Path, Message: field.Message})
	}
	return converted
}
