// Package config defines the torrentfs configuration model.
//
// M1 defines the paths and connections sections. TOML loading, validation
// and the proxy/cache sections land in M4.
package config

import (
	"os"
	"path/filepath"
)

// Config holds torrentfs runtime settings.
type Config struct {
	Paths       Paths
	Connections Connections
}

// Paths groups the filesystem paths torrentfs manages at runtime.
type Paths struct {
	// DataDir is where the torrent session stores and reads torrent data.
	// It must exist and be writable; the session creates it if missing.
	DataDir string
}

// Connections groups network settings of the torrent session.
type Connections struct {
	// ListenHost is the address the session listens on for peer
	// connections. Empty lets the client pick its default.
	ListenHost string
	// ListenPort is the TCP/UDP port for incoming peer connections.
	// 0 selects a random free port.
	ListenPort int
}

// Default returns the default configuration: a per-user data directory under
// the OS cache dir (falling back to the temp dir) and an ephemeral listen
// port.
func Default() Config {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return Config{
		Paths: Paths{
			DataDir: filepath.Join(base, "torrentfs"),
		},
		Connections: Connections{
			ListenHost: "",
			ListenPort: 0,
		},
	}
}
