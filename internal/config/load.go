package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Load strictly decodes TOML, overlays the documented environment, and validates.
func Load(path string) (Config, error) {
	cfg, _, err := LoadWithSources(path)
	return cfg, err
}

// LoadWithSources loads the configuration and reports where every effective
// value came from.
//
// The sources let a caller tell a value the user wrote in a configuration
// file from a value the environment supplied, without repeating the merge.
// The credential warning needs that distinction; a configuration audit needs
// it for every key.
func LoadWithSources(path string) (Config, Sources, error) {
	return loadWithSources(path, osEnvironment{})
}

// LoadWithEnvironment loads the configuration from a given environment.
//
// The environment is a parameter so a caller can supply the variables it
// knows about instead of the whole process environment. The configuration
// reads the same variables either way.
func LoadWithEnvironment(path string, lookup Environment) (Config, error) {
	cfg, _, err := loadWithSources(path, lookup)
	return cfg, err
}

// LoadContentWithSources loads a configuration document held in memory and
// reports where every effective value came from. name identifies the document
// in error messages; the caller that validates a rendered write-back passes
// the configuration file path, so a problem names the file the user edits.
func LoadContentWithSources(content []byte, name string) (Config, Sources, error) {
	return loadContentWithSources(content, name, osEnvironment{})
}

// LoadContentWithEnvironment loads a configuration document held in memory
// from a given environment.
func LoadContentWithEnvironment(content []byte, name string, lookup Environment) (Config, error) {
	cfg, _, err := loadContentWithSources(content, name, lookup)
	return cfg, err
}

// LoadContentFileLayer loads only the file configuration layer of a document:
// the built-in defaults overlaid with the document, without the environment and
// without the value or requirement rules.
//
// The layer is what the write-back renderer, the retired-sidecar migrations,
// and the configuration page read. It is deliberately not validated here: a
// requirement can hold only for the effective configuration (for example the
// SMB credentials may come from the environment), so the rules must run on the
// document rendered through the real environment, not on the file alone.
func LoadContentFileLayer(content []byte, name string) (Config, Sources, error) {
	cfg := Default()
	sources := fileSources()
	removed, err := decodeContentLayer(content, name, &cfg, sources)
	if err != nil {
		return Config{}, nil, err
	}
	if len(removed) > 0 {
		return Config{}, nil, fmt.Errorf("config: %w", &RemovedError{Names: removed})
	}
	return cfg, sources, nil
}

func load(path string, lookup envLookup) (Config, error) {
	cfg, _, err := loadWithSources(path, lookup)
	return cfg, err
}

func loadWithSources(path string, lookup envLookup) (Config, Sources, error) {
	if path != "" {
		content, err := os.ReadFile(path)
		if err != nil {
			return Config{}, nil, fmt.Errorf("config: open %q: %w", path, err)
		}
		return loadContentWithSources(content, path, lookup)
	}
	return loadContentWithSources(nil, "", lookup)
}

// loadContentWithSources loads a configuration document held in memory.
//
// A document loaded from disk and a document the configuration page rendered
// travel through this function, so the value rules, the environment merge and
// the sources cannot drift apart. name identifies the document in error
// messages; an empty name means no document at all.
func loadContentWithSources(content []byte, name string, lookup envLookup) (Config, Sources, error) {
	cfg := Default()
	sources := fileSources()
	// A removed key in the file has to stop the load before the strict
	// decoder runs, because the decoder would report it as unknown. A removed
	// environment name does not: it is reported with the other problems, so a
	// user who also has a bad value learns about both at once.
	envRemoved := removedEnvironmentNames(lookup)
	fileRemoved, err := decodeContentLayer(content, name, &cfg, sources)
	if err != nil {
		return Config{}, nil, err
	}
	if len(fileRemoved) > 0 {
		return Config{}, nil, fmt.Errorf("config: %w", &RemovedError{Names: append(envRemoved, fileRemoved...)})
	}
	// The environment and the values are both checked, and both sets of
	// errors are reported together: a malformed environment value and an
	// unusable configuration value are the same kind of mistake, and a user
	// who set both learns about both at once.
	collector := &validationCollector{}
	collector.add(unknownEnvironmentError(lookup))
	if err := applyEnvironment(&cfg, lookup, sources); err != nil {
		collector.add(err)
	}
	collector.add(cfg.Validate())
	// The loaded values travel with the error so a caller can run the checks
	// that live outside this package (the output rule and the rules of the
	// upload statistics experiment) and report every problem at once. A
	// failed decode leaves the zero value: there are no values to check then.
	validateErr := collector.err()
	if validateErr != nil {
		if name == "" {
			validateErr = fmt.Errorf("config: validate: %w", validateErr)
		} else {
			validateErr = fmt.Errorf("config: validate %q: %w", name, validateErr)
		}
	}
	if len(envRemoved) > 0 {
		removedErr := fmt.Errorf("config: %w", &RemovedError{Names: envRemoved})
		if validateErr == nil {
			return cfg, sources, removedErr
		}
		return cfg, sources, errors.Join(removedErr, validateErr)
	}
	if validateErr != nil {
		return cfg, sources, validateErr
	}
	return cfg, sources, nil
}

// fileSources returns the source record of the file configuration layer before
// any input is read: every configuration key starts at its built-in default.
func fileSources() Sources {
	sources := Sources{}
	for _, item := range configSchema {
		sources[item.path] = SourceDefault
	}
	return sources
}

// decodeContentLayer decodes the file layer of a document onto cfg and reports
// the removed configuration names the document carries.
//
// The removed names are reported before the strict decoder runs, because the
// decoder would report a removed key as unknown without naming its replacement.
func decodeContentLayer(content []byte, name string, cfg *Config, sources Sources) ([]RemovedName, error) {
	if name == "" {
		return nil, nil
	}
	document, err := decodeDocument(content, name)
	if err != nil {
		return nil, err
	}
	if removed := removedTOMLNames(document); len(removed) > 0 {
		return removed, nil
	}
	set, err := decodeFile(content, name, cfg)
	if err != nil {
		return nil, err
	}
	for key := range documentKeys(set) {
		sources[key] = SourceFile
	}
	return nil, nil
}

// decodeDocument decodes a configuration file into a generic tree.
//
// The generic tree names the keys a document sets, including the keys that no
// longer exist: the strict decoder would report an unknown table without
// saying which setting replaces it.
func decodeDocument(content []byte, path string) (map[string]any, error) {
	var document map[string]any
	if err := toml.Unmarshal(content, &document); err != nil {
		return nil, fmt.Errorf("config: decode %q: %w", path, err)
	}
	return document, nil
}

// decodeFile decodes one configuration file onto the default configuration and
// reports the configuration keys the document sets.
func decodeFile(content []byte, path string, cfg *Config) (map[string]any, error) {
	document, err := decodeDocument(content, path)
	if err != nil {
		return nil, err
	}
	if err := toml.NewDecoder(bytes.NewReader(content)).DisallowUnknownFields().Decode(cfg); err != nil {
		var strictErr *toml.StrictMissingError
		if errors.As(err, &strictErr) {
			if strings.Contains(strings.ToLower(strictErr.String()), "password") {
				return nil, errors.New("config: unknown password field; use the top-level username and password keys")
			}
			return nil, fmt.Errorf("config: decode %q: %s: %w", path, strictErr.String(), err)
		}
		var decodeErr *toml.DecodeError
		if errors.As(err, &decodeErr) {
			message := decodeErr.String()
			if row, _ := decodeErr.Position(); row > 0 {
				if key := keyRows(content).keyAt(row); key != "" {
					message += fmt.Sprintf("; the configuration key %q holds the value", key)
				}
			}
			if strings.Contains(message, "CapacityBytes") {
				message += "; cache.capacity must be a quoted byte quantity such as 32MB or 2GiB"
			}
			if strings.Contains(message, "MaxUploadBytes") {
				message += "; http.max_upload_size must be a quoted byte quantity such as 10MiB or 32MB"
			}
			return nil, fmt.Errorf("config: decode %q: %s: %w", path, message, err)
		}
		return nil, fmt.Errorf("config: decode %q: %w", path, err)
	}
	return document, nil
}

// documentKeys returns the dot paths of every leaf a decoded TOML document
// sets. A table contributes its leaves, not itself, so a document that sets
// "http.auth.enabled" reports that path and not "http.auth".
func documentKeys(document map[string]any) map[string]any {
	keys := make(map[string]any)
	collectDocumentKeys(document, "", keys)
	return keys
}

// collectDocumentKeys walks one level of a decoded document and records the
// paths of its leaves.
func collectDocumentKeys(table map[string]any, prefix string, keys map[string]any) {
	for name, value := range table {
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		if nested, ok := value.(map[string]any); ok {
			collectDocumentKeys(nested, path, keys)
			continue
		}
		keys[path] = value
	}
}
