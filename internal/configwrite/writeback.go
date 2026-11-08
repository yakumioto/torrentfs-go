// Package configwrite performs one configuration write-back.
//
// The configuration page save and both startup sidecar migrations share the
// same steps: read the file layer through the loader a start uses, apply a
// sparse change, render the document through the write-back renderer, and
// validate the rendered document with the process rules. The package keeps
// those steps in one place, so the three callers cannot drift apart on which
// loader validates the result or on how a write-back reports a failure.
package configwrite

import (
	"errors"
	"fmt"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/configcheck"
)

// File is the file configuration layer of one document: the values the file
// carries over the built-in defaults, and the source of every value.
type File struct {
	Config  config.Config
	Sources config.Sources
}

// Load reads the file configuration layer of content. name identifies the
// document in error messages, so a problem names the file the user edits.
//
// The layer is loaded without the environment and without the value or
// requirement rules: a requirement can hold only for the effective
// configuration (for example the SMB credentials may come from the
// environment), so the rules run on the document rendered through the real
// environment instead.
func Load(content []byte, name string) (File, error) {
	cfg, sources, err := config.LoadContentFileLayer(content, name)
	if err != nil {
		return File{}, err
	}
	return File{Config: cfg, Sources: sources}, nil
}

// Prepared is a rendered write-back and the effective configuration a start
// would produce from it.
type Prepared struct {
	// Changed lists the configuration keys the write-back adds or replaces,
	// in schema order.
	Changed []string
	// Candidate is the file configuration layer after the change; it is what
	// the controller keeps as the file view once the write-back commits.
	Candidate config.Config
	// Document is the rendered configuration file.
	Document []byte
	// Effective and EffectiveSources are the configuration and the source of
	// every value after loading Document through the same loader and process
	// rules as a start.
	Effective        config.Config
	EffectiveSources config.Sources
}

// Prepare applies a sparse text patch to the file layer and renders the
// write-back.
//
// onlyMissing skips a patch key the file already carries, which is the
// migration rule: a value the operator wrote by hand wins over a retired
// sidecar. The rendered document is validated by configcheck, so the process
// rules and the experimental cross-field rules apply exactly as at a start.
//
// A patch value that does not parse does not stop the other keys from being
// validated: the parse errors and the validation errors are joined, so one
// call reports every problem.
func Prepare(file File, name string, patch map[string]string, onlyMissing bool) (Prepared, error) {
	candidate := file.Config
	var changed []string
	var parseErr error
	for _, key := range config.Schema() {
		text, ok := patch[key.Path]
		if !ok {
			continue
		}
		if onlyMissing && file.Sources.FromFile(key.Path) {
			continue
		}
		if err := config.SetText(&candidate, key.Path, text); err != nil {
			parseErr = errors.Join(parseErr, err)
			continue
		}
		changed = append(changed, key.Path)
	}
	prepared, renderErr := Render(candidate, file.Sources, changed, name)
	return prepared, errors.Join(parseErr, renderErr)
}

// Render renders the write-back of a candidate configuration and validates it.
//
// It is the direct-candidate form of Prepare: the experiment migration merges
// typed sidecar values into the configuration instead of formatting them as
// text, and the rendered document follows the same renderer and validator.
func Render(candidate config.Config, sources config.Sources, changed []string, name string) (Prepared, error) {
	document, err := config.WriteBack{File: candidate, Sources: sources, Changed: changed}.TOML()
	if err != nil {
		return Prepared{Changed: changed, Candidate: candidate}, err
	}
	effective, effectiveSources, err := configcheck.LoadContent(document, name)
	if err != nil {
		return Prepared{Changed: changed, Candidate: candidate, Document: document}, err
	}
	return Prepared{
		Changed:          changed,
		Candidate:        candidate,
		Document:         document,
		Effective:        effective,
		EffectiveSources: effectiveSources,
	}, nil
}

// Publish replaces the configuration file with document.
//
// writeBackFile is the atomic writer; nil uses the configuration package
// writer. A writer that reports no publication without an error is a failed
// write-back, so a caller never reports success for a file that did not change.
func Publish(name string, document []byte, writeBackFile func(string, []byte) (bool, error)) (bool, error) {
	if writeBackFile == nil {
		writeBackFile = config.WriteBackFile
	}
	published, err := writeBackFile(name, document)
	if !published && err == nil {
		err = fmt.Errorf("configwrite: the write-back of %q did not publish the configuration file", name)
	}
	return published, err
}
