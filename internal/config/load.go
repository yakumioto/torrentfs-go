package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"
)

// Load reads, strictly decodes, overlays environment variables, and validates
// a TOML configuration file. An empty path loads only the defaults and
// environment variables.
func Load(path string) (Config, error) {
	return load(path, os.LookupEnv)
}

func load(path string, lookup envLookup) (Config, error) {
	cfg := Default()
	if path != "" {
		file, err := os.Open(path)
		if err != nil {
			return Config{}, fmt.Errorf("config: open %q: %w", path, err)
		}
		defer func() { _ = file.Close() }()

		if err := toml.NewDecoder(file).DisallowUnknownFields().Decode(&cfg); err != nil {
			var strictErr *toml.StrictMissingError
			if errors.As(err, &strictErr) {
				return Config{}, fmt.Errorf("config: decode %q: %s: %w", path, strictErr.String(), err)
			}
			var decodeErr *toml.DecodeError
			if errors.As(err, &decodeErr) {
				return Config{}, fmt.Errorf("config: decode %q: %s: %w", path, decodeErr.String(), err)
			}
			return Config{}, fmt.Errorf("config: decode %q: %w", path, err)
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
