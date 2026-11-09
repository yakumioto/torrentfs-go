// Package configcheck loads and validates the configuration exactly as the
// process start does.
//
// The value rules live in the configuration package; the process rule (at
// least one output) and the cross-field rules of the upload statistics
// experiment live above it. A start and the configuration page both call this
// package, so the page cannot accept a configuration a start would reject, and
// the two report the same wording.
package configcheck

import (
	"errors"
	"fmt"
	"strings"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/experiments"
)

// Load loads a configuration file, merges the process environment, and
// applies the process rules.
//
// The loaded configuration and its sources travel with the error: a caller
// that reports per-key problems keeps the values that were read.
func Load(path string) (config.Config, config.Sources, error) {
	cfg, sources, err := config.LoadWithSources(path)
	return cfg, sources, Check(cfg, err)
}

// LoadContent loads a configuration document held in memory, merges the
// process environment, and applies the process rules.
//
// The configuration page validates a rendered write-back through this
// function before it replaces the configuration file. name identifies the file
// the document belongs to, so a rejected value names the file the user edits.
func LoadContent(content []byte, name string) (config.Config, config.Sources, error) {
	cfg, sources, err := config.LoadContentWithSources(content, name)
	return cfg, sources, Check(cfg, err)
}

// Check applies the process rules to a loaded configuration.
//
// loadErr is the error of the load, if any. It is reported with the process
// rules instead of replacing them, so one run names every problem. A failed
// decode leaves the zero configuration, which selects no backend: the process
// rule passes and nothing is claimed about values that were never read. A
// validation error travels with the loaded values.
func Check(cfg config.Config, loadErr error) error {
	problems := []error{loadErr, cfg.ValidateOutput()}
	// A rejected experiment value is one problem. The cross-field rules would
	// restate it in other words, so they wait for the next run.
	if loadErr == nil || !configurationNamesExperiment(loadErr) {
		problems = append(problems, checkExperiment(cfg))
	}
	return errors.Join(problems...)
}

// configurationNamesExperiment reports whether the configuration already
// rejected an experiment setting.
func configurationNamesExperiment(err error) bool {
	var validationErrs *config.ValidationErrors
	if !errors.As(err, &validationErrs) {
		return false
	}
	for _, field := range validationErrs.Fields() {
		if strings.HasPrefix(field, "experiments.upload.") {
			return true
		}
	}
	return false
}

// ExperimentError reports a cross-field rule of the upload statistics
// experiment.
//
// The rules relate one setting to another, so the error has no single key to
// attribute the problem to. It carries the section instead, which is the
// stable place the configuration page shows the problem.
type ExperimentError struct {
	// Err is the rule the experiment reported.
	Err error
}

// Error renders the experiment rule the way a start reports it.
func (e *ExperimentError) Error() string {
	return fmt.Sprintf("config: experiments.upload experiment: %v", e.Err)
}

// Unwrap returns the experiment rule so errors.Is and errors.As reach it.
func (e *ExperimentError) Unwrap() error { return e.Err }

// checkExperiment reports the rules of the upload statistics experiment that
// relate one setting to another.
//
// The rules live in the experiment package, because they decide what a set of
// settings means. The configuration package cannot import it without a cycle,
// so the check lives here, above both.
func checkExperiment(cfg config.Config) error {
	if _, err := experiments.SettingsFrom(cfg.Experiments.Upload); err != nil {
		return &ExperimentError{Err: err}
	}
	return nil
}

// FieldError is one problem of a rejected configuration.
type FieldError struct {
	// Path is the dot path of the configuration key, or empty when the
	// problem belongs to no single key (a removed key, an unreadable file).
	Path string
	// Message is the problem without the configuration origin prefix.
	Message string
}

// FieldErrors returns one entry per rejected configuration key.
//
// The page shows each entry next to its field, so a save report and a startup
// report stay the same diagnosis in the same words. A problem that belongs to
// no single key keeps an empty path and its whole message.
func FieldErrors(err error) []FieldError {
	if err == nil {
		return nil
	}
	collector := &fieldErrorCollector{}
	collector.add(err)
	return collector.errors
}

// fieldErrorCollector gathers the problems of one error tree.
type fieldErrorCollector struct {
	errors []FieldError
}

// add records the problems of one error, descending into the errors a load
// joins and wraps.
func (c *fieldErrorCollector) add(err error) {
	if err == nil {
		return
	}
	switch typed := err.(type) {
	case *config.ValidationErrors:
		for _, validationErr := range typed.Errors {
			c.errors = append(c.errors, FieldError{Path: validationErr.Field, Message: validationErr.Cause.Error()})
		}
		return
	case *config.ValidationError:
		c.errors = append(c.errors, FieldError{Path: typed.Field, Message: typed.Cause.Error()})
		return
	case *ExperimentError:
		c.errors = append(c.errors, FieldError{Path: "experiments.upload", Message: typed.Err.Error()})
		return
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, inner := range joined.Unwrap() {
			c.add(inner)
		}
		return
	}
	if inner := errors.Unwrap(err); inner != nil {
		// A wrapper around a per-key report keeps the keys; a wrapper around
		// a problem without a key keeps the wrapper's message, which names
		// the origin the inner error does not carry.
		before := len(c.errors)
		c.add(inner)
		if len(c.errors) == before {
			c.errors = append(c.errors, FieldError{Message: err.Error()})
		}
		return
	}
	c.errors = append(c.errors, FieldError{Message: err.Error()})
}
