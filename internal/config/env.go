package config

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

type envLookup func(string) (string, bool)

type envBinding struct {
	name  string
	field string
	apply func(*Config, string) error
}

var environmentBindings = []envBinding{
	{
		name:  "TORRENTFS_CONNECTIONS_LISTEN_HOST",
		field: "connections.listen_host",
		apply: func(cfg *Config, raw string) error {
			cfg.Connections.ListenHost = raw
			return nil
		},
	},
	{
		name:  "TORRENTFS_CONNECTIONS_LISTEN_PORT",
		field: "connections.listen_port",
		apply: func(cfg *Config, raw string) error {
			value, err := parseEnvInt(raw)
			if err != nil {
				return err
			}
			cfg.Connections.ListenPort = value
			return nil
		},
	},
	{
		name:  "TORRENTFS_CONNECTIONS_DISABLE_IPV4",
		field: "connections.disable_ipv4",
		apply: func(cfg *Config, raw string) error {
			value, err := parseEnvBool(raw)
			if err != nil {
				return err
			}
			cfg.Connections.DisableIPv4 = value
			return nil
		},
	},
	{
		name:  "TORRENTFS_CONNECTIONS_DISABLE_IPV6",
		field: "connections.disable_ipv6",
		apply: func(cfg *Config, raw string) error {
			value, err := parseEnvBool(raw)
			if err != nil {
				return err
			}
			cfg.Connections.DisableIPv6 = value
			return nil
		},
	},
	{
		name:  "TORRENTFS_CONNECTIONS_NO_PORT_FORWARDING",
		field: "connections.no_port_forwarding",
		apply: func(cfg *Config, raw string) error {
			value, err := parseEnvBool(raw)
			if err != nil {
				return err
			}
			cfg.Connections.NoPortForwarding = value
			return nil
		},
	},
	{
		name:  "TORRENTFS_CONNECTIONS_BOOTSTRAP_NODES",
		field: "connections.bootstrap_nodes",
		apply: func(cfg *Config, raw string) error {
			cfg.Connections.BootstrapNodes = parseEnvList(raw)
			return nil
		},
	},
	{
		name:  "TORRENTFS_PROXY_SOCKS5_URL",
		field: "proxy.socks5_url",
		apply: func(cfg *Config, raw string) error {
			cfg.Proxy.Socks5URL = raw
			return nil
		},
	},
	{
		name:  "TORRENTFS_CACHE_CAPACITY_BYTES",
		field: "cache.capacity_bytes",
		apply: func(cfg *Config, raw string) error {
			value, err := parseEnvInt64(raw)
			if err != nil {
				return err
			}
			cfg.Cache.CapacityBytes = value
			return nil
		},
	},
	{
		name:  "TORRENTFS_IDENTITY_TRACKER_USER_AGENT",
		field: "identity.tracker_user_agent",
		apply: func(cfg *Config, raw string) error {
			cfg.Identity.TrackerUserAgent = raw
			return nil
		},
	},
	{
		name:  "TORRENTFS_IDENTITY_PEER_ID_PREFIX",
		field: "identity.peer_id_prefix",
		apply: func(cfg *Config, raw string) error {
			cfg.Identity.PeerIDPrefix = raw
			return nil
		},
	},
	{
		name:  "TORRENTFS_IDENTITY_EXTENDED_HANDSHAKE_CLIENT_VERSION",
		field: "identity.extended_handshake_client_version",
		apply: func(cfg *Config, raw string) error {
			cfg.Identity.ExtendedHandshakeClientVersion = raw
			return nil
		},
	},
	{
		name:  "TORRENTFS_HTTP_LISTEN_ADDR",
		field: "http.listen_addr",
		apply: func(cfg *Config, raw string) error {
			cfg.HTTP.ListenAddr = raw
			return nil
		},
	},
	{
		name:  "TORRENTFS_HTTP_MAX_UPLOAD_BYTES",
		field: "http.max_upload_bytes",
		apply: func(cfg *Config, raw string) error {
			value, err := parseEnvInt64(raw)
			if err != nil {
				return err
			}
			cfg.HTTP.MaxUploadBytes = value
			return nil
		},
	},
	{
		name:  "TORRENTFS_HTTP_AUTH_ENABLED",
		field: "http.auth.enabled",
		apply: func(cfg *Config, raw string) error {
			value, err := parseEnvBool(raw)
			if err != nil {
				return err
			}
			cfg.HTTP.Auth.Enabled = value
			return nil
		},
	},
	{
		name:  "TORRENTFS_HTTP_AUTH_USERNAME",
		field: "http.auth.username",
		apply: func(cfg *Config, raw string) error {
			cfg.HTTP.Auth.Username = raw
			return nil
		},
	},
	{
		name:  "TORRENTFS_HTTP_AUTH_PASSWORD_HASH",
		field: "http.auth.password_hash",
		apply: func(cfg *Config, raw string) error {
			cfg.HTTP.Auth.PasswordHash = raw
			return nil
		},
	},
	{
		name:  "TORRENTFS_HTTP_AUTH_PASSWORD_HASH_FILE",
		field: "http.auth.password_hash_file",
		apply: func(cfg *Config, raw string) error {
			cfg.HTTP.Auth.PasswordHashFile = raw
			return nil
		},
	},
	{
		name:  "TORRENTFS_HTTP_AUTH_TOKEN_TTL",
		field: "http.auth.token_ttl",
		apply: func(cfg *Config, raw string) error {
			value, err := parseEnvDuration(raw)
			if err != nil {
				return err
			}
			cfg.HTTP.Auth.TokenTTL = value
			return nil
		},
	},
	{
		name:  "TORRENTFS_LOG_LEVEL",
		field: "log.level",
		apply: func(cfg *Config, raw string) error {
			cfg.Log.Level = raw
			return nil
		},
	},
	{
		name:  "TORRENTFS_LOG_FORMAT",
		field: "log.format",
		apply: func(cfg *Config, raw string) error {
			cfg.Log.Format = raw
			return nil
		},
	},
	{
		name:  "TORRENTFS_LOG_ADD_SOURCE",
		field: "log.add_source",
		apply: func(cfg *Config, raw string) error {
			value, err := parseEnvBool(raw)
			if err != nil {
				return err
			}
			cfg.Log.AddSource = value
			return nil
		},
	},
}

func applyEnvironment(cfg *Config, lookup envLookup) error {
	for _, binding := range environmentBindings {
		raw, ok := lookup(binding.name)
		if !ok {
			continue
		}
		if err := binding.apply(cfg, raw); err != nil {
			return invalid(binding.field, errors.New("environment variable "+binding.name+": "+err.Error()))
		}
	}
	return nil
}

// parseEnvList splits a comma-separated environment value. Blank entries are
// dropped so a trailing comma is not a configuration error.
func parseEnvList(raw string) []string {
	var values []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			values = append(values, item)
		}
	}
	return values
}

func parseEnvInt(raw string) (int, error) {
	value, err := strconv.ParseInt(raw, 10, 0)
	if err != nil {
		return 0, errors.New("must be a base-10 integer")
	}
	return int(value), nil
}

func parseEnvInt64(raw string) (int64, error) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, errors.New("must be a base-10 integer")
	}
	return value, nil
}

func parseEnvBool(raw string) (bool, error) {
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, errors.New("must be a boolean")
	}
	return value, nil
}

func parseEnvDuration(raw string) (Duration, error) {
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, errors.New("must be a Go duration")
	}
	return Duration(value), nil
}
