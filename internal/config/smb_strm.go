package config

import "fmt"

// SMBSTRMEnabled selects the optional projection independently of the original
// share. Without an explicit switch, a configured streaming base URL enables it.
func (c Config) SMBSTRMEnabled() bool {
	return c.Filesystem.Backend == BackendSMB && c.STRM.BaseURL != "" &&
		(c.SMB.STRMEnabled == nil || *c.SMB.STRMEnabled)
}

// applySMBSTRMEnvironment applies the environment value of the STRM share
// switch. The switch is only meaningful for the SMB output.
func applySMBSTRMEnvironment(c *Config, values map[string]any) error {
	value, ok := values[smbSTRMEnabledPath]
	if !ok {
		return nil
	}
	enabled, ok := value.(*bool)
	if !ok {
		return invalid(smbSTRMEnabledPath, fmt.Errorf("the environment value of %s is not a boolean",
			deriveEnvironmentName(smbSTRMEnabledPath)))
	}
	if *enabled && c.Filesystem.Backend != BackendSMB {
		return invalid(smbSTRMEnabledPath, fmt.Errorf("%s requires the SMB backend",
			deriveEnvironmentName(smbSTRMEnabledPath)))
	}
	c.SMB.STRMEnabled = enabled
	return nil
}
