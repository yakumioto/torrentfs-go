// Package config defines the torrentfs configuration model.
package config

// Config holds torrentfs runtime settings. It is intentionally empty in the
// M0 skeleton; fields are added as later milestones pin down the schema
// (paths/connections in M1, proxy/cache/loading in M4).
type Config struct{}

// Default returns the default configuration.
func Default() Config {
	return Config{}
}
