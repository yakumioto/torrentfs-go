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

func load(path string, lookup envLookup) (Config, error) {
	cfg, _, err := loadWithSources(path, lookup)
	return cfg, err
}

func loadWithSources(path string, lookup envLookup) (Config, Sources, error) {
	cfg := Default()
	sources := Sources{}
	// Every configuration key starts at its built-in default, so the record
	// names a source for every key even when no input touched it.
	for _, item := range configSchema {
		sources[item.path] = SourceDefault
	}
	if path != "" {
		content, err := os.ReadFile(path)
		if err != nil {
			return Config{}, nil, fmt.Errorf("config: open %q: %w", path, err)
		}
		set, err := decodeFile(content, path, &cfg)
		if err != nil {
			return Config{}, nil, err
		}
		for key := range documentKeys(set) {
			sources[key] = SourceFile
		}
	}
	if err := applyEnvironment(&cfg, lookup, sources); err != nil {
		return Config{}, nil, fmt.Errorf("config: apply environment: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		if path == "" {
			return Config{}, nil, fmt.Errorf("config: validate: %w", err)
		}
		return Config{}, nil, fmt.Errorf("config: validate %q: %w", path, err)
	}
	return cfg, sources, nil
}

// decodeFile decodes one configuration file onto the default configuration and
// reports the configuration keys the document sets.
//
// The document is read twice: once into a generic tree to recognize removed
// names, and once strictly into the configuration itself.
func decodeFile(content []byte, path string, cfg *Config) (map[string]any, error) {
	var document map[string]any
	if err := toml.Unmarshal(content, &document); err != nil {
		return nil, fmt.Errorf("config: decode %q: %w", path, err)
	}
	if removed := removedTOMLNames(document); len(removed) > 0 {
		return nil, fmt.Errorf("config: decode %q: %w", path, &RemovedError{Names: removed})
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
