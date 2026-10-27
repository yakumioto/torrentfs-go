package config

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

func (c Config) validateFilesystem() error {
	if c.Filesystem.Mode != "fuse" && c.Filesystem.Mode != "smb" {
		return invalid("filesystem.mode", errors.New("must be fuse or smb"))
	}
	_, port, err := net.SplitHostPort(c.SMB.ListenAddr)
	if err != nil {
		return invalid("smb.listen_addr", errListenAddr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return invalid("smb.listen_addr", errPortRange)
	}
	name := c.SMB.ShareName
	if name == "" || name == "." || name == ".." || !utf8.ValidString(name) || strings.EqualFold(name, "IPC$") || strings.ContainsAny(name, "/\\:;|=,+*?<>\"") || strings.IndexFunc(name, unicode.IsControl) >= 0 || strings.TrimSpace(name) != name || len(name) > 80 {
		return invalid("smb.share_name", errors.New("must be a share name of 1..80 bytes without separators, reserved characters or IPC$"))
	}
	if c.Filesystem.Mode == "smb" {
		if c.SMB.Username == "" || c.SMB.Password == "" {
			return invalid("smb.credentials", errors.New("SMB requires non-empty TORRENTFS_USERNAME and TORRENTFS_PASSWORD together"))
		}
		if strings.ContainsAny(c.SMB.Username, "\\/:") || strings.IndexFunc(c.SMB.Username, unicode.IsControl) >= 0 {
			return invalid("smb.username", errAuthUsernameControl)
		}
		if c.SMB.STRMEnabled && (c.STRM.BaseURL == "" || c.HTTP.ListenAddr == "") {
			return invalid("smb.strm_enabled", errors.New("requires strm.base_url and the HTTP service"))
		}
	}
	return nil
}

func applyLegacyFilesystemEnvironment(cfg *Config, lookup envLookup) error {
	if _, canonical := lookup("TORRENTFS_FILESYSTEM_MODE"); !canonical {
		if raw, set := lookup("TORRENTFS_SMB_ENABLED"); set {
			enabled, err := parseEnvBool(raw)
			if err != nil {
				return invalid("filesystem.mode", errors.New("environment variable TORRENTFS_SMB_ENABLED: must be a boolean"))
			}
			cfg.Filesystem.Mode = "fuse"
			if enabled {
				cfg.Filesystem.Mode = "smb"
			}
		}
	}
	if _, canonical := lookup("TORRENTFS_SMB_STRM_ENABLED"); !canonical {
		if raw, set := lookup("TORRENTFS_STRM_ENABLED"); set {
			enabled, err := parseEnvBool(raw)
			if err != nil {
				return invalid("smb.strm_enabled", errors.New("environment variable TORRENTFS_STRM_ENABLED: must be a boolean"))
			}
			cfg.SMB.STRMEnabled = enabled
		}
	}
	return nil
}
