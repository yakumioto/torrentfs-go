// Package config defines torrentfs runtime settings and their TOML loader.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var (
	// ErrInvalid identifies a configuration value that violates its contract.
	ErrInvalid = errors.New("invalid configuration")

	errRequired         = errors.New("value is required")
	errPortRange        = errors.New("must be between 0 and 65535")
	errCapacityRange    = errors.New("must be non-negative")
	errProxyScheme      = errors.New("must use socks5:// or socks5h://")
	errProxyHost        = errors.New("must include a proxy host")
	errProxyPort        = errors.New("proxy port must be between 1 and 65535")
	errPeerIDPrefixSize = errors.New("must be at most 20 bytes")
	errTrackerUserAgent = errors.New("must not contain carriage return or line feed")
)

const defaultCacheCapacityBytes int64 = 64 << 20

// ValidationError identifies the configuration field that failed validation.
type ValidationError struct {
	Field string
	Cause error
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("config: %s: %v", e.Field, e.Cause)
}

func (e *ValidationError) Unwrap() error {
	return e.Cause
}

func (e *ValidationError) Is(target error) bool {
	return target == ErrInvalid
}

func invalid(field string, cause error) error {
	return &ValidationError{Field: field, Cause: cause}
}

// Config holds torrentfs runtime settings.
type Config struct {
	Paths       Paths       `toml:"paths"`
	Connections Connections `toml:"connections"`
	Proxy       Proxy       `toml:"proxy"`
	Cache       Cache       `toml:"cache"`
	Identity    Identity    `toml:"identity"`
}

// Paths groups the filesystem paths torrentfs manages at runtime.
type Paths struct {
	// DataDir is where the torrent session stores and reads torrent data.
	// It must exist and be writable; the session creates it if missing.
	DataDir string `toml:"data_dir"`
}

// Connections groups network settings of the torrent session.
type Connections struct {
	// ListenHost is the address the session listens on for peer
	// connections. Empty lets the client pick its default.
	ListenHost string `toml:"listen_host"`
	// ListenPort is the TCP/UDP port for incoming peer connections.
	// 0 selects a random free port.
	ListenPort int `toml:"listen_port"`
}

// Proxy groups optional outbound proxy settings.
type Proxy struct {
	// Socks5URL is an optional SOCKS5 or SOCKS5H endpoint.
	Socks5URL string `toml:"socks5_url"`
}

// Cache groups in-memory piece cache settings.
type Cache struct {
	// CapacityBytes is the maximum number of bytes retained in memory.
	CapacityBytes int64 `toml:"capacity_bytes"`
}

// Identity groups the client identity values sent to trackers and peers.
type Identity struct {
	// TrackerUserAgent is sent as the User-Agent header on HTTP tracker announces.
	TrackerUserAgent string `toml:"tracker_user_agent"`
	// PeerIDPrefix is the prefix of the generated 20-byte BitTorrent peer ID.
	PeerIDPrefix string `toml:"peer_id_prefix"`
	// ExtendedHandshakeClientVersion is sent as the BEP 10 extended handshake v field.
	ExtendedHandshakeClientVersion string `toml:"extended_handshake_client_version"`
}

// Default returns the default configuration: a per-user data directory under
// the OS cache dir (falling back to the temp dir), an ephemeral listen port,
// an empty proxy, and a 64 MiB in-memory piece cache.
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
		Cache: Cache{
			CapacityBytes: defaultCacheCapacityBytes,
		},
	}
}

// Validate checks values that must be valid before starting a session.
func (c Config) Validate() error {
	if c.Paths.DataDir == "" {
		return invalid("paths.data_dir", errRequired)
	}
	if c.Connections.ListenPort < 0 || c.Connections.ListenPort > 65535 {
		return invalid("connections.listen_port", errPortRange)
	}
	if c.Cache.CapacityBytes < 0 {
		return invalid("cache.capacity_bytes", errCapacityRange)
	}
	if err := validateProxyURL(c.Proxy.Socks5URL); err != nil {
		return invalid("proxy.socks5_url", err)
	}
	if len(c.Identity.PeerIDPrefix) > 20 {
		return invalid("identity.peer_id_prefix", errPeerIDPrefixSize)
	}
	if strings.ContainsAny(c.Identity.TrackerUserAgent, "\r\n") {
		return invalid("identity.tracker_user_agent", errTrackerUserAgent)
	}
	return nil
}

func validateProxyURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if !strings.EqualFold(u.Scheme, "socks5") && !strings.EqualFold(u.Scheme, "socks5h") {
		return errProxyScheme
	}
	if u.Host == "" || u.Hostname() == "" {
		return errProxyHost
	}
	if strings.HasSuffix(u.Host, ":") {
		return errProxyPort
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return errProxyPort
		}
	}
	return nil
}
