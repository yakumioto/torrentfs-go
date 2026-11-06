package config

// SMBSTRMEnabled reports whether the SMB output offers the STRM share.
//
// The switch has three states: "auto" provides the share when a streaming
// base URL is configured, "on" forces it, and "off" forbids it. The share
// exists only for the SMB output.
func (c Config) SMBSTRMEnabled() bool {
	if c.Backend != BackendSMB {
		return false
	}
	switch c.SMB.STRM {
	case STRMOff:
		return false
	case STRMOn:
		return true
	default:
		return c.STRM.BaseURL != ""
	}
}
