package config

import (
	"errors"
	"strings"
)

// ValidationErrors reports every configuration value that violates its
// contract. A user who changed several values should learn about all of them
// at once instead of one restart per mistake.
//
// The errors are ordered by the configuration key they name, so the report
// reads the same way on every run.
type ValidationErrors struct {
	// Errors holds one entry per invalid configuration key.
	Errors []*ValidationError
}

// Error renders one line per invalid configuration key.
func (e *ValidationErrors) Error() string {
	messages := make([]string, 0, len(e.Errors))
	for _, validationErr := range e.Errors {
		messages = append(messages, validationErr.Error())
	}
	return strings.Join(messages, "\n")
}

// Unwrap returns the underlying errors so errors.Is and errors.As reach them.
func (e *ValidationErrors) Unwrap() []error {
	unwrapped := make([]error, 0, len(e.Errors))
	for _, validationErr := range e.Errors {
		unwrapped = append(unwrapped, validationErr)
	}
	return unwrapped
}

// Is reports that an invalid configuration is the reason.
func (e *ValidationErrors) Is(target error) bool { return target == ErrInvalid }

// HasField reports whether one of the errors names the given configuration
// key. It lets a caller check a single key without reading the whole report.
func (e *ValidationErrors) HasField(field string) bool {
	for _, validationErr := range e.Errors {
		if validationErr.Field == field {
			return true
		}
	}
	return false
}

// Fields returns the configuration keys the errors name, in report order.
func (e *ValidationErrors) Fields() []string {
	fields := make([]string, 0, len(e.Errors))
	for _, validationErr := range e.Errors {
		fields = append(fields, validationErr.Field)
	}
	return fields
}

// validationCollector gathers the errors of one configuration load so that the
// load can report all of them together.
type validationCollector struct {
	errors []*ValidationError
}

// add records one invalid configuration key. A nil error is ignored, so the
// caller can pass a check's result straight in.
//
// An error that already holds several invalid keys contributes all of them,
// so aggregating two reports does not lose the entries of either.
func (c *validationCollector) add(err error) {
	if err == nil {
		return
	}
	var validationErrs *ValidationErrors
	if errors.As(err, &validationErrs) {
		c.errors = append(c.errors, validationErrs.Errors...)
		return
	}
	var validationErr *ValidationError
	if errors.As(err, &validationErr) {
		c.errors = append(c.errors, validationErr)
		return
	}
	c.errors = append(c.errors, &ValidationError{Field: "configuration", Cause: err})
}

// err returns the collected errors as one error, or nil when nothing failed.
func (c *validationCollector) err() error {
	if len(c.errors) == 0 {
		return nil
	}
	return &ValidationErrors{Errors: c.errors}
}
