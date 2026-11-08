package configctl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/configcheck"
	"github.com/yakumioto/torrentfs-go/internal/configwrite"
)

// DefaultReloadInterval is how often Watch checks the configuration file for
// an external change.
const DefaultReloadInterval = 2 * time.Second

// missingFileDigest marks a poll that could not read the configuration file.
// It cannot collide with a SHA-256 hex digest, so a file that reappears is
// always a change the next poll examines.
const missingFileDigest = "-"

// ReloadResult is the outcome of one external-change check.
type ReloadResult struct {
	// Changed reports whether the file bytes differed from the last content
	// the controller handled. A rejected edit reports true: the file did
	// change, it just could not be adopted.
	Changed bool
	// Applied lists the changed keys that took effect in the running process,
	// in schema order.
	Applied []string
	// RestartRequired lists the changed keys that the file now holds but the
	// running process keeps using the previous value of until the next start,
	// in schema order.
	RestartRequired []string
}

// CheckExternalChange checks the configuration file once and reloads it when
// its bytes differ from the last content the controller handled.
//
// The reload reuses the load, validation and apply path of a save: the
// document is loaded through configcheck, so the environment and the
// cross-field rules apply exactly as at a start, and only the keys whose value
// actually changed reach a component applier. A restart-only change stays in
// the file view and in the status; the running configuration keeps its value.
//
// A file that cannot be read or validated leaves the running configuration and
// the file view on the last known good values, records the failure in
// last_reload, and is reported as the returned error. The failure is recorded
// once per file version: the next poll of the same bytes does nothing.
//
// A save updates the baseline to the bytes it wrote, so the poll after a save
// finds no change and neither applies the values again nor logs them.
func (c *Controller) CheckExternalChange() (ReloadResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.path == "" {
		return ReloadResult{}, nil
	}

	content, err := os.ReadFile(c.path)
	if err != nil {
		message := fmt.Sprintf("configctl: read the configuration file: %v", err)
		if c.fileError != message || c.observed != missingFileDigest {
			c.observed = missingFileDigest
			c.fileError = message
			c.recordReloadLocked(false, message, nil)
			c.logger.Error("configuration reload failed",
				slog.String("config_file", c.path), "err", err)
		}
		return ReloadResult{}, errors.New(message)
	}

	digest := configurationBaseline(content)
	if digest == c.observed {
		// The bytes are the ones the last check already handled. A rejected
		// edit keeps failing, but it is not recorded or logged again; a valid
		// one was applied once.
		if c.fileError != "" {
			return ReloadResult{}, errors.New(c.fileError)
		}
		return ReloadResult{}, nil
	}
	// Remember the bytes before loading them: a rejected edit is one failure,
	// not one failure per poll until the file changes again.
	c.observed = digest

	file, err := configwrite.Load(content, c.path)
	if err != nil {
		return c.failReloadLocked(err)
	}
	fileConfig := file.Config
	effective, effectiveSources, err := configcheck.LoadContent(content, c.path)
	if err != nil {
		return c.failReloadLocked(err)
	}

	// The file view adopts every change that validated. The running
	// configuration adopts only the changed values that took effect: a
	// restart-only key, and a key the environment overrides, keep the value
	// the process already uses.
	changed := changedKeys(c.fileConfig, fileConfig)
	applicable := changedEffectiveKeys(c.cfg, effective, changed)
	applied, restartRequired := c.applyLocked(effective, applicable)
	c.cfg, c.sources = runningConfiguration(c.cfg, c.sources, effective, effectiveSources, applied)
	c.fileConfig = fileConfig
	c.baseline = digest
	c.fileError = ""

	now := time.Now().UTC()
	c.lastReload = Status{At: &now, OK: true, Keys: changed}
	c.logger.Info("configuration reloaded",
		slog.String("config_file", c.path),
		slog.String("keys", strings.Join(changed, ",")),
		slog.String("applied", strings.Join(applied, ",")),
		slog.String("restart_required", strings.Join(restartRequired, ",")))
	return ReloadResult{Changed: true, Applied: applied, RestartRequired: restartRequired}, nil
}

// failReloadLocked records a rejected external edit. The caller holds the
// lock. The file view and the running configuration stay on the last known
// good values.
func (c *Controller) failReloadLocked(cause error) (ReloadResult, error) {
	c.fileError = cause.Error()
	c.recordReloadLocked(false, cause.Error(), nil)
	c.logger.Error("configuration reload failed",
		slog.String("config_file", c.path), "err", cause)
	return ReloadResult{Changed: true}, cause
}

// recordReloadLocked records the outcome of the most recent reload. The caller
// holds the lock.
func (c *Controller) recordReloadLocked(ok bool, message string, keys []string) {
	now := time.Now().UTC()
	if keys == nil {
		keys = []string{}
	}
	c.lastReload = Status{At: &now, OK: ok, Error: message, Keys: keys}
}

// Watch polls the configuration file until ctx is done and reloads an
// external change it finds.
//
// A deployment that started without a configuration file has nothing to
// watch, so Watch returns immediately. Watch does not depend on the HTTP API:
// a headless deployment with a configuration file reloads the same way.
func (c *Controller) Watch(ctx context.Context) {
	if c.path == "" {
		return
	}
	ticks, stop := c.newTicker(c.reloadInterval)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			// The failure is recorded in the status and logged by the check
			// itself; the poll continues with the next file version.
			_, _ = c.CheckExternalChange()
		}
	}
}

// tickerFactory produces the tick channel of the poll and its stop function.
//
// The interval is the production poll period; the tests replace the factory
// with one they send on, so they drive the poll without waiting for it.
type tickerFactory func(time.Duration) (<-chan time.Time, func())

// productionTicker is the real poll clock.
func productionTicker(interval time.Duration) (<-chan time.Time, func()) {
	ticker := time.NewTicker(interval)
	return ticker.C, ticker.Stop
}

// changedKeys returns the configuration keys whose file value differs between
// two configurations, in schema order.
func changedKeys(previous, next config.Config) []string {
	changed := make([]string, 0, len(config.Schema()))
	for _, key := range config.Schema() {
		before, _ := config.ValueText(previous, key.Path)
		after, _ := config.ValueText(next, key.Path)
		if before != after {
			changed = append(changed, key.Path)
		}
	}
	return changed
}

// changedEffectiveKeys returns the keys of candidates whose effective value
// changed between two configurations, in schema order.
//
// It keeps an externally edited key the environment overrides out of the
// apply path: the file value changed, but the running process already uses the
// environment value and has nothing to receive.
func changedEffectiveKeys(previous config.Config, next config.Config, candidates []string) []string {
	inCandidates := make(map[string]bool, len(candidates))
	for _, path := range candidates {
		inCandidates[path] = true
	}
	changed := make([]string, 0, len(candidates))
	for _, key := range config.Schema() {
		if !inCandidates[key.Path] {
			continue
		}
		before, _ := config.ValueText(previous, key.Path)
		after, _ := config.ValueText(next, key.Path)
		if before != after {
			changed = append(changed, key.Path)
		}
	}
	return changed
}
