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

// Backend names the output backend: the way the file tree reaches the user.
type Backend string

// The output backends of torrentfs.
const (
	// BackendNone serves no file tree: only the HTTP API stays.
	BackendNone Backend = "none"
	// BackendFUSE serves the file tree as a read-only FUSE mount.
	BackendFUSE Backend = "fuse"
	// BackendSMB serves the file tree as a read-only SMB share.
	BackendSMB Backend = "smb"
)

// STRMSwitch names the state of the STRM share of the SMB output.
type STRMSwitch string

// The states of the STRM share of the SMB output.
const (
	// STRMAuto provides the share when a streaming base URL is configured.
	STRMAuto STRMSwitch = "auto"
	// STRMOn always provides the share; it requires a streaming base URL.
	STRMOn STRMSwitch = "on"
	// STRMOff never provides the share.
	STRMOff STRMSwitch = "off"
)

const (
	defaultReadTimeout = Duration(30 * time.Second)

	// backendPath is the configuration key that selects the output backend.
	backendPath = "backend"
	// smbSTRMPath is the configuration key that switches the STRM share of
	// the SMB output on or off.
	smbSTRMPath = "smb.strm"
)

// Filesystem groups the settings that describe the file output as a whole.
type Filesystem struct {
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
	// STRM switches the STRM share of the SMB output: auto, on, or off.
	STRM STRMSwitch `toml:"strm" enum:"auto,on,off"`
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

// validateFilesystem checks the settings of the selected output backend. The
// settings of an unselected backend are retained so switching only the
// backend is sufficient, but they are not checked: an unused mountpoint does
// not have to exist.
//
// The value of an enumerated key is always checked, even when its backend is
// not selected: an unusable value is a mistake wherever it stands.
func (c Config) validateFilesystem() error {
	if err := validateEnum(backendPath, string(c.Backend)); err != nil {
		return err
	}
	if c.Filesystem.ReadTimeout <= 0 {
		return invalid("filesystem.read_timeout", errPositive)
	}
	if err := validateEnum(smbSTRMPath, string(c.SMB.STRM)); err != nil {
		return err
	}
	if c.Backend == BackendFUSE {
		if c.FUSE.Mountpoint == "" && c.FUSE.STRMMountpoint == "" {
			return invalid("fuse.mountpoint", errors.New("at least one FUSE mountpoint is required"))
		}
		if c.FUSE.STRMMountpoint != "" && c.STRM.BaseURL == "" {
			return invalid("fuse.strm_mountpoint", errors.New("requires strm.base_url and HTTP"))
		}
	}
	if c.Backend != BackendSMB {
		return nil
	}
	if c.SMB.STRM == STRMOn && c.STRM.BaseURL == "" {
		return invalid(smbSTRMPath, errors.New("requires strm.base_url and the HTTP listener"))
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

// validateEnum reports whether a value is one of the accepted values of an
// enumerated configuration key. The accepted values come from the schema, so
// the validator and the environment parser name the same list.
func validateEnum(path, value string) error {
	values, ok := schemaEnumValues(path)
	if !ok {
		return nil
	}
	if err := acceptEnumValue(values, value); err != nil {
		return invalid(path, err)
	}
	return nil
}
