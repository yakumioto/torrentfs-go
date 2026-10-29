package config

import (
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	BackendNone        = "none"
	BackendFUSE        = "fuse"
	BackendSMB         = "smb"
	defaultReadTimeout = Duration(30 * time.Second)
)

type Filesystem struct {
	Backend     string   `toml:"backend"`
	ReadTimeout Duration `toml:"read_timeout"`
}

type FUSE struct {
	Mountpoint     string `toml:"mountpoint"`
	STRMMountpoint string `toml:"strm_mountpoint"`
	AllowOther     bool   `toml:"allow_other"`
}

type SMB struct {
	ListenAddr   string `toml:"listen_addr"`
	Username     string `toml:"username"`
	PasswordFile string `toml:"password_file"`
	Password     string `toml:"-"`
	// Nil selects the STRM share automatically when strm.base_url is set.
	STRMEnabled *bool `toml:"strm_enabled"`
}

type fileFilesystem struct {
	Backend     *string   `toml:"backend"`
	ReadTimeout *Duration `toml:"read_timeout"`
}

type fileFUSE struct {
	Mountpoint     string `toml:"mountpoint"`
	STRMMountpoint string `toml:"strm_mountpoint"`
	AllowOther     *bool  `toml:"allow_other"`
}

type fileMount struct {
	AllowOther  *bool     `toml:"allow_other"`
	ReadTimeout *Duration `toml:"read_timeout"`
}

func (c fileConfig) outputConfig(cfg *Config) {
	cfg.Filesystem = Filesystem{Backend: BackendNone, ReadTimeout: defaultReadTimeout}
	cfg.SMB = c.SMB
	if cfg.SMB.ListenAddr == "" {
		cfg.SMB.ListenAddr = "0.0.0.0:1445"
	}
	cfg.FUSE = FUSE{Mountpoint: c.FUSE.Mountpoint, STRMMountpoint: c.FUSE.STRMMountpoint}
	if c.Filesystem.Backend != nil {
		cfg.Filesystem.Backend = *c.Filesystem.Backend
		cfg.backendExplicit = true
	}
	if c.Filesystem.ReadTimeout != nil {
		cfg.Filesystem.ReadTimeout = *c.Filesystem.ReadTimeout
	}
	if c.Mount.ReadTimeout != nil {
		if c.Filesystem.ReadTimeout != nil && *c.Filesystem.ReadTimeout != *c.Mount.ReadTimeout {
			cfg.outputError = invalid("filesystem.read_timeout", errors.New("conflicts with mount.read_timeout"))
		}
		cfg.Filesystem.ReadTimeout = *c.Mount.ReadTimeout
	}
	if c.FUSE.AllowOther != nil {
		cfg.FUSE.AllowOther = *c.FUSE.AllowOther
	}
	if c.Mount.AllowOther != nil {
		if c.FUSE.AllowOther != nil && *c.FUSE.AllowOther != *c.Mount.AllowOther {
			cfg.outputError = invalid("fuse.allow_other", errors.New("conflicts with mount.allow_other"))
		}
		cfg.FUSE.AllowOther = *c.Mount.AllowOther
	}
	cfg.syncMount()
}

func (c *Config) syncMount() {
	c.Mount = Mount{AllowOther: c.FUSE.AllowOther, ReadTimeout: c.Filesystem.ReadTimeout}
}

// FileReadTimeout preserves the in-process legacy Mount setting for existing
// Backend callers; loaded configuration always normalizes both representations.
func (c Config) FileReadTimeout() Duration {
	if c.Filesystem.ReadTimeout == defaultReadTimeout && c.Mount.ReadTimeout != defaultReadTimeout {
		return c.Mount.ReadTimeout
	}
	return c.Filesystem.ReadTimeout
}

func applyFilesystemEnvironment(c *Config, lookup envLookup) error {
	if raw, ok := lookup("TORRENTFS_FILESYSTEM_BACKEND"); ok {
		c.Filesystem.Backend, c.backendExplicit = raw, true
	}
	if raw, ok := lookup("TORRENTFS_SMB_ENABLED"); ok && raw != "" {
		enabled, err := parseEnvBool(raw)
		if err != nil {
			return invalid("filesystem.backend", errors.New("TORRENTFS_SMB_ENABLED must be a boolean"))
		}
		if enabled {
			if c.backendExplicit && c.Filesystem.Backend != BackendSMB {
				return invalid("filesystem.backend", errors.New("conflicts with TORRENTFS_SMB_ENABLED"))
			}
			c.Filesystem.Backend, c.backendExplicit = BackendSMB, true
		} else if c.backendExplicit && c.Filesystem.Backend == BackendSMB {
			return invalid("filesystem.backend", errors.New("conflicts with TORRENTFS_SMB_ENABLED"))
		}
	}
	for name, target := range map[string]*string{
		"TORRENTFS_FUSE_MOUNTPOINT":      &c.FUSE.Mountpoint,
		"TORRENTFS_FUSE_STRM_MOUNTPOINT": &c.FUSE.STRMMountpoint,
		"TORRENTFS_SMB_LISTEN_ADDR":      &c.SMB.ListenAddr,
		"TORRENTFS_SMB_USERNAME":         &c.SMB.Username,
		"TORRENTFS_SMB_PASSWORD_FILE":    &c.SMB.PasswordFile,
	} {
		if raw, ok := lookup(name); ok {
			*target = raw
		}
	}
	newTimeout, hasNewTimeout := lookup("TORRENTFS_FILESYSTEM_READ_TIMEOUT")
	_, hasOldTimeout := lookup("TORRENTFS_MOUNT_READ_TIMEOUT")
	if hasNewTimeout {
		value, err := parseEnvDuration(newTimeout)
		if err != nil {
			return invalid("filesystem.read_timeout", errors.New("invalid TORRENTFS_FILESYSTEM_READ_TIMEOUT"))
		}
		if hasOldTimeout && value != c.Mount.ReadTimeout {
			return invalid("filesystem.read_timeout", errors.New("conflicts with TORRENTFS_MOUNT_READ_TIMEOUT"))
		}
		c.Filesystem.ReadTimeout = value
	} else if hasOldTimeout {
		c.Filesystem.ReadTimeout = c.Mount.ReadTimeout
	}
	newOther, hasNewOther := lookup("TORRENTFS_FUSE_ALLOW_OTHER")
	_, hasOldOther := lookup("TORRENTFS_MOUNT_ALLOW_OTHER")
	if hasNewOther {
		value, err := parseEnvBool(newOther)
		if err != nil {
			return invalid("fuse.allow_other", errors.New("invalid TORRENTFS_FUSE_ALLOW_OTHER"))
		}
		if hasOldOther && value != c.Mount.AllowOther {
			return invalid("fuse.allow_other", errors.New("conflicts with TORRENTFS_MOUNT_ALLOW_OTHER"))
		}
		c.FUSE.AllowOther = value
	} else if hasOldOther {
		c.FUSE.AllowOther = c.Mount.AllowOther
	}
	c.syncMount()
	if !c.backendExplicit && (c.FUSE.Mountpoint != "" || c.FUSE.STRMMountpoint != "") {
		c.Filesystem.Backend = BackendFUSE
	}
	if err := applySMBSTRMEnvironment(c, lookup); err != nil {
		return err
	}
	if c.Filesystem.Backend == BackendSMB {
		username, userSet := lookup(httpUsernameEnvironment)
		password, passwordSet := lookup(httpPasswordEnvironment)
		if userSet != passwordSet {
			return invalid("smb", errors.New("TORRENTFS_USERNAME and TORRENTFS_PASSWORD must be set together"))
		}
		if userSet {
			c.SMB.Username, c.SMB.Password = username, password
		} else if c.SMB.PasswordFile != "" {
			file, err := os.Open(c.SMB.PasswordFile)
			if err != nil {
				return invalid("smb.password_file", errors.New("cannot open password file"))
			}
			bytes, readErr := io.ReadAll(io.LimitReader(file, 65537))
			closeErr := file.Close()
			if readErr != nil || closeErr != nil || len(bytes) > 65536 {
				return invalid("smb.password_file", errors.New("cannot read password file (maximum 64 KiB)"))
			}
			c.SMB.Password = strings.TrimSuffix(strings.TrimSuffix(string(bytes), "\n"), "\r")
		}
	}
	return nil
}

// ResolveFilesystem applies legacy CLI aliases after loading TOML/env. Settings
// of an unselected backend are retained so switching only backend is sufficient.
func (c *Config) ResolveFilesystem(mountpoint, strmMountpoint string) error {
	if mountpoint != "" || strmMountpoint != "" {
		if c.backendExplicit && c.Filesystem.Backend != BackendFUSE {
			return invalid("filesystem.backend", errors.New("FUSE CLI mountpoints conflict with selected backend"))
		}
		c.Filesystem.Backend = BackendFUSE
		if mountpoint != "" {
			c.FUSE.Mountpoint = mountpoint
		}
		if strmMountpoint != "" {
			c.FUSE.STRMMountpoint = strmMountpoint
		}
	}
	if c.Filesystem.Backend == BackendFUSE {
		if c.FUSE.Mountpoint == "" && c.FUSE.STRMMountpoint == "" {
			return invalid("fuse.mountpoint", errors.New("at least one FUSE mountpoint is required"))
		}
		if c.FUSE.STRMMountpoint != "" && c.STRM.BaseURL == "" {
			return invalid("fuse.strm_mountpoint", errors.New("requires strm.base_url and HTTP"))
		}
	}
	return c.validateFilesystem()
}

func (c Config) validateFilesystem() error {
	if c.outputError != nil {
		return c.outputError
	}
	switch c.Filesystem.Backend {
	case BackendNone, BackendFUSE, BackendSMB:
	default:
		return invalid("filesystem.backend", errors.New("must be fuse, smb, or none"))
	}
	if c.Filesystem.ReadTimeout <= 0 {
		return invalid("filesystem.read_timeout", errPositive)
	}
	if c.Filesystem.Backend != BackendSMB {
		return nil
	}
	if c.SMB.STRMEnabled != nil && *c.SMB.STRMEnabled && c.STRM.BaseURL == "" {
		return invalid("smb.strm_enabled", errors.New("requires strm.base_url and the HTTP listener"))
	}
	if c.SMB.Username == "" || strings.ContainsFunc(c.SMB.Username, unicode.IsControl) {
		return invalid("smb.username", errors.New("must be nonempty and contain no control characters"))
	}
	if c.SMB.Password == "" || !utf8.ValidString(c.SMB.Password) || strings.ContainsAny(c.SMB.Password, "\r\n\x00") {
		return invalid("smb", errors.New("a nonempty, single-line password is required"))
	}
	_, port, err := net.SplitHostPort(c.SMB.ListenAddr)
	if err != nil || port == "" {
		return invalid("smb.listen_addr", errListenAddr)
	}
	value, err := parseEnvInt(port)
	if err != nil || value < 1 || value > 65535 {
		return invalid("smb.listen_addr", errListenAddr)
	}
	return nil
}
