package config

import "errors"

// SMBSTRMEnabled selects the optional projection independently of the original
// share. Without an explicit switch, a configured streaming base URL enables it.
func (c Config) SMBSTRMEnabled() bool {
	return c.Filesystem.Backend == BackendSMB && c.STRM.BaseURL != "" &&
		(c.SMB.STRMEnabled == nil || *c.SMB.STRMEnabled)
}

func applySMBSTRMEnvironment(c *Config, lookup envLookup) error {
	canonical, hasCanonical := lookup("TORRENTFS_SMB_STRM_ENABLED")
	legacy, hasLegacy := lookup("TORRENTFS_STRM_ENABLED")
	var selected *bool
	if hasCanonical {
		value, err := parseEnvBool(canonical)
		if err != nil {
			return invalid("smb.strm_enabled", errors.New("invalid TORRENTFS_SMB_STRM_ENABLED"))
		}
		selected = &value
	}
	if hasLegacy {
		value, err := parseEnvBool(legacy)
		if err != nil {
			return invalid("smb.strm_enabled", errors.New("invalid TORRENTFS_STRM_ENABLED"))
		}
		if selected != nil && *selected != value {
			return invalid("smb.strm_enabled", errors.New("conflicts with TORRENTFS_STRM_ENABLED"))
		}
		if value && c.Filesystem.Backend != BackendSMB {
			return invalid("smb.strm_enabled", errors.New("TORRENTFS_STRM_ENABLED requires the SMB backend"))
		}
		selected = &value
	}
	if selected != nil {
		c.SMB.STRMEnabled = selected
	}
	return nil
}
