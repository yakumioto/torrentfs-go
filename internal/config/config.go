// Package config defines torrentfs runtime settings and their TOML loader.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	// ErrInvalid identifies a configuration value that violates its contract.
	ErrInvalid = errors.New("invalid configuration")

	errRequired         = errors.New("value is required")
	errPortRange        = errors.New("must be between 0 and 65535")
	errCapacityRange    = errors.New("must be between 1 and the cache watermark safety limit")
	errProxyURL         = errors.New("must be a valid URL")
	errProxyScheme      = errors.New("must use socks5:// or socks5h://")
	errProxyHost        = errors.New("must include a proxy host")
	errProxyPort        = errors.New("proxy port must be between 1 and 65535")
	errPeerIDPrefixSize = errors.New("must be at most 20 bytes")
	errTrackerUserAgent = errors.New("must not contain carriage return or line feed")
	errListenAddr       = errors.New("must be a host:port address")
	errPositive         = errors.New("must be positive")
	errAddressFamilies  = errors.New("must not disable IPv4 and IPv6 at the same time")
	errBootstrapNode    = errors.New("must be a host:port address with a port between 1 and 65535")
	errAuthTokenTTLMax  = errors.New("must not exceed 24 hours")
	errExposedNoAuth    = errors.New("exposing a non-loopback address requires enabled http.auth")
	errLogLevel         = errors.New("must be one of debug, info, warn, error")
	errLogFormat        = errors.New("must be one of text, json")
)

const (
	defaultCacheCapacityBytes int64 = 2 << 30
	defaultHTTPListenAddr           = "127.0.0.1:8080"
	defaultSMBListenAddr            = "0.0.0.0:1445"
	defaultMaxUploadBytes     int64 = 10 << 20
	defaultLogLevel                 = "info"
	defaultLogFormat                = "text"

	// maxPasswordBytes is the longest password bcrypt accepts. A longer
	// password is truncated by bcrypt, so the configuration rejects it
	// instead of silently accepting a weaker password than the user wrote.
	maxPasswordBytes = 72
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
	// Backend selects the output backend: none, fuse, or smb.
	Backend Backend `toml:"backend" enum:"none,fuse,smb"`
	// Username is the login name the SMB output accepts. The HTTP login does
	// not check a user name, so the key is only required for the SMB output.
	Username string `toml:"username"`
	// Password is the plaintext password shared by the SMB output and the
	// HTTP login. It is required for the SMB output and for enabled HTTP
	// authentication, and it has no default.
	Password string `toml:"password" secret:"true"`
	// Connections groups the network settings of the torrent session.
	Connections Connections `toml:"connections"`
	// Proxy groups the optional outbound proxy settings.
	Proxy Proxy `toml:"proxy"`
	// Cache groups the in-memory piece cache settings.
	Cache Cache `toml:"cache"`
	// Identity groups the client identity values.
	Identity Identity `toml:"identity"`
	// HTTP groups the optional torrent-management HTTP service settings.
	HTTP HTTP `toml:"http"`
	// STRM groups the optional signed HTTP streaming settings.
	STRM STRM `toml:"strm"`
	// Log groups the process log settings.
	Log Log `toml:"log"`
	// Filesystem groups the settings that describe the file output as a whole.
	Filesystem Filesystem `toml:"filesystem"`
	// FUSE groups the settings of the read-only FUSE file output.
	FUSE FUSE `toml:"fuse"`
	// SMB groups the settings of the built-in read-only SMB file output.
	SMB SMB `toml:"smb"`
}

// HTTP groups the optional torrent-management HTTP service settings.
type HTTP struct {
	// ListenAddr is the address the HTTP service binds. Empty disables the
	// service. The default binds loopback only.
	ListenAddr string `toml:"listen_addr"`
	// Auth configures the optional single-user HTTP authentication service.
	Auth Auth `toml:"auth"`
	// MaxUploadBytes caps any uploaded request body: a .torrent upload or a
	// managed subtitle file.
	MaxUploadBytes ByteQuantity `toml:"max_upload_size"`
}

// STRM groups the optional signed HTTP streaming settings.
type STRM struct {
	// BaseURL is the player-reachable HTTP(S) URL, optionally with a reverse
	// proxy path prefix. Empty disables signed streaming links.
	BaseURL string `toml:"base_url"`
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
//
// The credentials live in the top-level username and password keys: the
// login checks the password only, so the authentication service needs no
// user name of its own.
type Auth struct {
	Enabled  bool     `toml:"enabled"`
	TokenTTL Duration `toml:"token_ttl"`
}

// Connections groups network settings of the torrent session.
type Connections struct {
	// ListenHost is the address the session listens on for peer
	// connections. Empty lets the client pick its default.
	ListenHost string `toml:"listen_host"`
	// ListenPort is the TCP/UDP port for incoming peer connections.
	// 0 selects a random free port.
	ListenPort int `toml:"listen_port"`
	// DisableIPv4 turns off every IPv4 listener, dialer, and DHT server.
	DisableIPv4 bool `toml:"disable_ipv4"`
	// DisableIPv6 turns off every IPv6 listener, dialer, and DHT server.
	// Only one family may be disabled: disabling both leaves no transport.
	DisableIPv6 bool `toml:"disable_ipv6"`
	// NoPortForwarding disables UPnP/NAT-PMP port mapping. It defaults to true
	// so a deployment with no UPnP device behaves the same as one that has a
	// device but no mapping rule.
	NoPortForwarding bool `toml:"no_port_forwarding"`
	// BootstrapNodes overrides the default DHT bootstrap hosts with explicit
	// host:port entries. Entries are resolved per address family and filtered
	// to the family of the socket that asks for them. Empty uses the
	// client's built-in bootstrap list.
	BootstrapNodes []string `toml:"bootstrap_nodes"`
}

// Proxy groups optional outbound proxy settings.
type Proxy struct {
	// Socks5URL is an optional SOCKS5 or SOCKS5H endpoint.
	Socks5URL string `toml:"socks5_url"`
}

// Cache groups in-memory piece cache settings.
type Cache struct {
	// CapacityBytes is the maximum number of bytes retained in memory.
	CapacityBytes ByteQuantity `toml:"capacity"`
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

// Default returns the default configuration: an ephemeral listen port, an
// empty proxy, qBittorrent 4.4.0 identity values, owner-only FUSE access, and
// a 2 GiB in-memory piece cache sized for a host with about 4 GB of RAM.
//
// This is the only place where configuration defaults are defined. A TOML
// document is decoded directly onto this value, so a configuration file that
// omits a key keeps the same default whether or not a file was loaded.
func Default() Config {
	return Config{
		Backend: BackendNone,
		Connections: Connections{
			ListenHost:       "",
			ListenPort:       0,
			NoPortForwarding: true,
		},
		Cache: Cache{
			CapacityBytes: ByteQuantity(defaultCacheCapacityBytes),
		},
		HTTP: HTTP{
			ListenAddr: defaultHTTPListenAddr,
			Auth: Auth{
				TokenTTL: Duration(30 * time.Minute),
			},
			MaxUploadBytes: ByteQuantity(defaultMaxUploadBytes),
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
		Filesystem: Filesystem{ReadTimeout: defaultReadTimeout},
		FUSE:       FUSE{AllowOther: false},
		SMB:        SMB{ListenAddr: defaultSMBListenAddr, STRM: STRMAuto},
	}
}

// Validate checks values that must be valid before starting a session.
func (c Config) Validate() error {
	if err := c.validateFilesystem(); err != nil {
		return err
	}
	if err := c.validateCredentials(); err != nil {
		return err
	}
	if c.Connections.ListenPort < 0 || c.Connections.ListenPort > 65535 {
		return invalid("connections.listen_port", errPortRange)
	}
	if c.Connections.DisableIPv4 && c.Connections.DisableIPv6 {
		return invalid("connections.disable_ipv4", errAddressFamilies)
	}
	for _, node := range c.Connections.BootstrapNodes {
		if err := validateBootstrapNode(node); err != nil {
			return invalid("connections.bootstrap_nodes", fmt.Errorf("%q: %w", node, err))
		}
	}
	if c.Cache.CapacityBytes <= 0 || c.Cache.CapacityBytes > math.MaxInt64/7 {
		return invalid("cache.capacity", errCapacityRange)
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
	if c.HTTP.ListenAddr != "" {
		if err := validateAuth(c.HTTP.Auth); err != nil {
			return err
		}
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
		return invalid("http.max_upload_size", errPositive)
	}
	if err := validateSTRM(c.STRM, c.HTTP.ListenAddr); err != nil {
		return invalid("strm.base_url", err)
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
	return nil
}

// validateCredentials checks the credentials against the selected output and
// the HTTP authentication.
//
// The password is required whenever a client has to authenticate against
// torrentfs: the SMB output and the HTTP login. The user name is required for
// the SMB output only, because the HTTP login checks the password alone.
//
// The shape of a credential is always checked, even when no output needs it
// yet: the credentials are shared by every output, so a malformed one is a
// mistake wherever it stands.
func (c Config) validateCredentials() error {
	if c.Username != "" && strings.IndexFunc(c.Username, unicode.IsControl) >= 0 {
		return invalid("username", errors.New("must not contain control characters"))
	}
	if c.Password != "" {
		if !utf8.ValidString(c.Password) {
			return invalid("password", errors.New("must be valid UTF-8"))
		}
		if strings.ContainsAny(c.Password, "\r\n\x00") {
			return invalid("password", errors.New("must be a single line without null bytes"))
		}
	}
	if c.Backend == BackendSMB && c.Username == "" {
		return invalid("username", errRequired)
	}
	if c.Backend == BackendSMB || c.HTTP.Auth.Enabled {
		if c.Password == "" {
			return invalid("password", errRequired)
		}
	}
	if c.HTTP.Auth.Enabled && len([]byte(c.Password)) > maxPasswordBytes {
		// The HTTP login hashes the password with bcrypt, which ignores
		// everything past the 72nd byte. A longer password would reach
		// the SMB output in full and the HTTP login truncated, so the
		// configuration rejects it instead.
		return invalid("password", fmt.Errorf("must be at most %d bytes", maxPasswordBytes))
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

// validateBootstrapNode checks one DHT bootstrap entry. Port 0 is rejected
// because a bootstrap host offering port 0 is a configuration mistake the
// resolver would otherwise turn into a silent no-op.
func validateBootstrapNode(raw string) error {
	host, port, err := net.SplitHostPort(raw)
	if err != nil || host == "" {
		return errBootstrapNode
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 1 || value > 65535 {
		return errBootstrapNode
	}
	return nil
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
