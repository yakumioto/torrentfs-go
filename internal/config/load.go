package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

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

		fileCfg := newFileConfig(cfg)
		if err := toml.NewDecoder(file).DisallowUnknownFields().Decode(&fileCfg); err != nil {
			var strictErr *toml.StrictMissingError
			if errors.As(err, &strictErr) {
				return Config{}, fmt.Errorf("config: decode %q: %s: %w", path, strictErr.String(), err)
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
				return Config{}, fmt.Errorf("config: decode %q: %s: %w", path, message, err)
			}
			return Config{}, fmt.Errorf("config: decode %q: %w", path, err)
		}
		cfg = fileCfg.runtimeConfig()
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

type fileConfig struct {
	Connections Connections `toml:"connections"`
	Proxy       Proxy       `toml:"proxy"`
	Cache       fileCache   `toml:"cache"`
	Identity    Identity    `toml:"identity"`
	HTTP        fileHTTP    `toml:"http"`
	Log         Log         `toml:"log"`
	Mount       Mount       `toml:"mount"`
}

type fileCache struct {
	CapacityBytes tomlByteQuantity `toml:"capacity"`
}

type fileHTTP struct {
	ListenAddr     string           `toml:"listen_addr"`
	Auth           Auth             `toml:"auth"`
	MaxUploadBytes tomlByteQuantity `toml:"max_upload_size"`
}

func newFileConfig(cfg Config) fileConfig {
	return fileConfig{
		Connections: cfg.Connections,
		Proxy:       cfg.Proxy,
		Cache:       fileCache{CapacityBytes: tomlByteQuantity(cfg.Cache.CapacityBytes)},
		Identity:    cfg.Identity,
		HTTP: fileHTTP{
			ListenAddr:     cfg.HTTP.ListenAddr,
			Auth:           cfg.HTTP.Auth,
			MaxUploadBytes: tomlByteQuantity(cfg.HTTP.MaxUploadBytes),
		},
		Log:   cfg.Log,
		Mount: cfg.Mount,
	}
}

func (c fileConfig) runtimeConfig() Config {
	return Config{
		Connections: c.Connections,
		Proxy:       c.Proxy,
		Cache:       Cache{CapacityBytes: int64(c.Cache.CapacityBytes)},
		Identity:    c.Identity,
		HTTP: HTTP{
			ListenAddr:     c.HTTP.ListenAddr,
			Auth:           c.HTTP.Auth,
			MaxUploadBytes: int64(c.HTTP.MaxUploadBytes),
		},
		Log:   c.Log,
		Mount: c.Mount,
	}
}
