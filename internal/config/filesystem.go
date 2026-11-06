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

// The configuration keys of the file output settings.
const (
	// outputBackendPath is the configuration key that selects the output
	// backend.
	outputBackendPath = "filesystem.backend"
	// smbSTRMEnabledPath is the configuration key that switches the STRM
	// share of the SMB output on or off.
	smbSTRMEnabledPath = "smb.strm_enabled"
)

// Filesystem groups the settings that describe the file output as a whole.
type Filesystem struct {
	// Backend selects the file output: none, fuse, or smb.
	Backend string `toml:"backend"`
	// ReadTimeout bounds a single read of a piece from the torrent.
	ReadTimeout Duration `toml:"read_timeout"`
}

// FUSE groups the settings of the read-only FUSE file output.
type FUSE struct {
	// Mountpoint is the directory the file tree is mounted on.
	Mountpoint string `toml:"mountpoint"`
	// STRMMountpoint is the directory the signed-streaming view is mounted on.
	STRMMountpoint string `toml:"strm_mountpoint"`
	// AllowOther passes allow_other to the kernel, lifting FUSE's default
	// restriction that only the mounting user may access the mount.
	AllowOther bool `toml:"allow_other"`
}

// SMB groups the settings of the built-in read-only SMB file output.
type SMB struct {
	// ListenAddr is the address the SMB service binds.
	ListenAddr string `toml:"listen_addr"`
	// Username is the login name the SMB service accepts.
	Username string `toml:"username"`
	// PasswordFile is a file holding the SMB password.
	PasswordFile string `toml:"password_file"`
	// Password is the plaintext SMB password. It is never read from TOML.
	Password string `toml:"-"`
	// Nil selects the STRM share automatically when strm.base_url is set.
	STRMEnabled *bool `toml:"strm_enabled"`
}

// readSMBPasswordFile reads the SMB password from the configured password
// file. The file is the fallback for a deployment that sets no shared
// credential pair.
func readSMBPasswordFile(c *Config) error {
	if c.SMB.PasswordFile == "" {
		return nil
	}
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
	return nil
}

// ResolveFilesystem completes the file output selection. Settings of an
// unselected backend are retained so switching only the backend is sufficient.
func (c *Config) ResolveFilesystem() error {
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
