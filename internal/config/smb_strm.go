package config

import "errors"

// SMBSTRMEnabled selects the optional projection independently of the original
// share. Without an explicit switch, a configured streaming base URL enables it.
func (c Config) SMBSTRMEnabled() bool {
	return c.Filesystem.Backend == BackendSMB && c.STRM.BaseURL != "" &&
		(c.SMB.STRMEnabled == nil || *c.SMB.STRMEnabled)
}

func applySMBSTRMEnvironment(c *Config, lookup envLookup) error {
	canonical, hasCanonical := lookup.Lookup(envSMBSTRMEnabled)
	if !hasCanonical {
		return nil
	}
	value, err := parseEnvBool(canonical)
	if err != nil {
		return invalid("smb.strm_enabled", errors.New("invalid "+envSMBSTRMEnabled))
	}
	if value && c.Filesystem.Backend != BackendSMB {
		return invalid("smb.strm_enabled", errors.New(envSMBSTRMEnabled+" requires the SMB backend"))
	}
	c.SMB.STRMEnabled = &value
	return nil
}
