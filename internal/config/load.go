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
func Load(path string) (Config, error) { return load(path, osEnvironment{}) }

func load(path string, lookup envLookup) (Config, error) {
	cfg := Default()
	if path != "" {
		content, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("config: open %q: %w", path, err)
		}
		if err := decodeFile(content, path, &cfg); err != nil {
			return Config{}, err
		}
	}
	if err := applyEnvironment(&cfg, lookup); err != nil {
		return Config{}, fmt.Errorf("config: apply environment: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		if path == "" {
			return Config{}, fmt.Errorf("config: validate: %w", err)
		}
		return Config{}, fmt.Errorf("config: validate %q: %w", path, err)
	}
	return cfg, nil
}

// decodeFile decodes one configuration file onto the default configuration.
// The document is read twice: once into a generic tree to recognize removed
// names and to learn which keys the document sets explicitly, and once
// strictly into the configuration itself.
func decodeFile(content []byte, path string, cfg *Config) error {
	var document map[string]any
	if err := toml.Unmarshal(content, &document); err != nil {
		return fmt.Errorf("config: decode %q: %w", path, err)
	}
	if removed := removedTOMLNames(document); len(removed) > 0 {
		return fmt.Errorf("config: decode %q: %w", path, &RemovedError{Names: removed})
	}
	// An explicitly selected file output wins over the mountpoint heuristic
	// that the environment applies.
	if documentUses(document, "filesystem.backend") {
		cfg.backendExplicit = true
	}
	if err := toml.NewDecoder(bytes.NewReader(content)).DisallowUnknownFields().Decode(cfg); err != nil {
		var strictErr *toml.StrictMissingError
		if errors.As(err, &strictErr) {
			if strings.Contains(strings.ToLower(strictErr.String()), "password") {
				return errors.New("config: unknown password field; use documented hash or secret-file settings")
			}
			return fmt.Errorf("config: decode %q: %s: %w", path, strictErr.String(), err)
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
			return fmt.Errorf("config: decode %q: %s: %w", path, message, err)
		}
		return fmt.Errorf("config: decode %q: %w", path, err)
	}
	return nil
}
