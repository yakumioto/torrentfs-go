// Package config defines torrentfs runtime settings and their TOML loader.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var (
	// ErrInvalid identifies a configuration value that violates its contract.
	ErrInvalid = errors.New("invalid configuration")

	errRequired             = errors.New("value is required")
	errPortRange            = errors.New("must be between 0 and 65535")
	errCapacityRange        = errors.New("must be non-negative")
	errProxyURL             = errors.New("must be a valid URL")
	errProxyScheme          = errors.New("must use socks5:// or socks5h://")
	errProxyHost            = errors.New("must include a proxy host")
	errProxyPort            = errors.New("proxy port must be between 1 and 65535")
	errPeerIDPrefixSize     = errors.New("must be at most 20 bytes")
	errTrackerUserAgent     = errors.New("must not contain carriage return or line feed")
	errListenAddr           = errors.New("must be a host:port address")
	errPositive             = errors.New("must be positive")
	errAuthUsernameRequired = errors.New("must not be empty when authentication is enabled")
	errAuthUsernameControl  = errors.New("must not contain control characters")
	errAuthHashRequired     = errors.New("exactly one password hash source is required when authentication is enabled")
	errAuthCredentialsSet   = errors.New("authentication credentials must be empty when authentication is disabled")
	errAuthTokenTTLMax      = errors.New("must not exceed 24 hours")
	errExposedNoAuth        = errors.New("exposing a non-loopback address requires enabled http.auth")
	errLogLevel             = errors.New("must be one of debug, info, warn, error")
	errLogFormat            = errors.New("must be one of text, json")
)

const (
	defaultCacheCapacityBytes int64 = 64 << 20
	defaultHTTPListenAddr           = "127.0.0.1:8080"
	defaultMaxUploadBytes     int64 = 10 << 20
	defaultLogLevel                 = "info"
	defaultLogFormat                = "text"
)

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

// ParseLogLevel parses one of the documented process log levels.
func ParseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, errLogLevel
	}
}

// Config holds torrentfs runtime settings.
type Config struct {
	Paths       Paths       `toml:"paths"`
	Connections Connections `toml:"connections"`
	Proxy       Proxy       `toml:"proxy"`
	Cache       Cache       `toml:"cache"`
	Identity    Identity    `toml:"identity"`
	HTTP        HTTP        `toml:"http"`
	Log         Log         `toml:"log"`
}

// Paths groups the filesystem paths torrentfs manages at runtime.
type Paths struct {
	// DataDir is where the torrent session stores and reads torrent data.
	// It must exist and be writable; the session creates it if missing.
	DataDir string `toml:"data_dir"`
	// PayloadDir is the managed root under which every torrent gets its own
	// per-info-hash directory. Empty uses <data_dir>/payload.
	PayloadDir string `toml:"payload_dir"`
}

// HTTP groups the optional torrent-management HTTP service settings.
type HTTP struct {
	// ListenAddr is the address the HTTP service binds. Empty disables the
	// service. The default binds loopback only.
	ListenAddr string `toml:"listen_addr"`
	// Auth configures the optional single-user HTTP authentication service.
	Auth Auth `toml:"auth"`
	// MaxUploadBytes caps an uploaded .torrent request body.
	MaxUploadBytes int64 `toml:"max_upload_bytes"`
}

// Log groups the process log settings.
type Log struct {
	Level     string `toml:"level"`
	Format    string `toml:"format"`
	AddSource bool   `toml:"add_source"`
}

// Duration is a time.Duration decoded from a TOML duration string.
type Duration time.Duration

// UnmarshalText decodes a Go duration string from TOML.
func (d *Duration) UnmarshalText(text []byte) error {
	value, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration: %w", err)
	}
	*d = Duration(value)
	return nil
}

// String returns the duration in Go's standard duration format.
func (d Duration) String() string {
	return time.Duration(d).String()
}

// Auth configures single-user password login and in-memory Bearer tokens.
type Auth struct {
	Enabled          bool     `toml:"enabled"`
	Username         string   `toml:"username"`
	PasswordHash     string   `toml:"password_hash"`
	PasswordHashFile string   `toml:"password_hash_file"`
	TokenTTL         Duration `toml:"token_ttl"`
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
// an empty proxy, qBittorrent 4.4.0 identity values, and a 64 MiB in-memory
// piece cache.
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
		HTTP: HTTP{
			ListenAddr: defaultHTTPListenAddr,
			Auth: Auth{
				TokenTTL: Duration(30 * time.Minute),
			},
			MaxUploadBytes: defaultMaxUploadBytes,
		},
		Identity: Identity{
			TrackerUserAgent:               "qBittorrent/4.4.0",
			PeerIDPrefix:                   "-qB4400-",
			ExtendedHandshakeClientVersion: "qBittorrent/4.4.0",
		},
		Log: Log{
			Level:  defaultLogLevel,
			Format: defaultLogFormat,
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
	if err := validateAuth(c.HTTP.Auth); err != nil {
		return err
	}
	if c.HTTP.ListenAddr != "" {
		host, _, err := net.SplitHostPort(c.HTTP.ListenAddr)
		if err != nil {
			return invalid("http.listen_addr", errListenAddr)
		}
		if !c.HTTP.Auth.Enabled && !isLoopbackHost(host) {
			return invalid("http.listen_addr", errExposedNoAuth)
		}
	}
	if c.HTTP.MaxUploadBytes <= 0 {
		return invalid("http.max_upload_bytes", errPositive)
	}
	if _, err := ParseLogLevel(c.Log.Level); err != nil {
		return invalid("log.level", errLogLevel)
	}
	if rawFormat := strings.TrimSpace(c.Log.Format); rawFormat != "" {
		switch strings.ToLower(rawFormat) {
		case "text", "json":
		default:
			return invalid("log.format", errLogFormat)
		}
	}
	return nil
}

func validateAuth(auth Auth) error {
	if auth.TokenTTL <= 0 {
		return invalid("http.auth.token_ttl", errPositive)
	}
	if time.Duration(auth.TokenTTL) > 24*time.Hour {
		return invalid("http.auth.token_ttl", errAuthTokenTTLMax)
	}

	hasPasswordHash := auth.PasswordHash != ""
	hasPasswordHashFile := auth.PasswordHashFile != ""
	if !auth.Enabled {
		switch {
		case auth.Username != "":
			return invalid("http.auth.username", errAuthCredentialsSet)
		case hasPasswordHash:
			return invalid("http.auth.password_hash", errAuthCredentialsSet)
		case hasPasswordHashFile:
			return invalid("http.auth.password_hash_file", errAuthCredentialsSet)
		default:
			return nil
		}
	}
	if auth.Username == "" {
		return invalid("http.auth.username", errAuthUsernameRequired)
	}
	if strings.IndexFunc(auth.Username, unicode.IsControl) >= 0 {
		return invalid("http.auth.username", errAuthUsernameControl)
	}
	if hasPasswordHash == hasPasswordHashFile {
		return invalid("http.auth.password_hash", errAuthHashRequired)
	}
	return nil
}

// isLoopbackHost reports whether an HTTP listen host is confined to the local
// machine. An empty host binds every interface and is therefore not loopback.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateProxyURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return &url.Error{Op: "parse", URL: "<redacted>", Err: errProxyURL}
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
