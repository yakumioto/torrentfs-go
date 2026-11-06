package config

import (
	"errors"
	"strings"
	"testing"
)

// TestSMBSTRMSwitchHasThreeStates covers the three states of the STRM share
// of the SMB output: auto follows the streaming base URL, on forces the share,
// and off forbids it.
func TestSMBSTRMSwitchHasThreeStates(t *testing.T) {
	tests := []struct {
		name  string
		state string
		base  string
		want  bool
	}{
		{name: "auto without a base URL", state: "auto", base: "", want: false},
		{name: "auto with a base URL", state: "auto", base: "http://media.example:8080", want: true},
		{name: "on with a base URL", state: "on", base: "http://media.example:8080", want: true},
		{name: "off with a base URL", state: "off", base: "http://media.example:8080", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := load(writeConfigFile(t, "backend = \"smb\"\n[strm]\nbase_url = \""+tt.base+"\"\n"+
				"[smb]\nstrm = \""+tt.state+"\"\n"),
				lookupEnvironment(map[string]string{
					environmentUsername: "smb-user",
					environmentPassword: "test-only-password",
				}))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := cfg.SMBSTRMEnabled(); got != tt.want {
				t.Fatalf("the STRM share is enabled = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSMBSTRMSwitchDefaultsToAuto covers the default of the switch: an
// omitted switch follows the streaming base URL.
func TestSMBSTRMSwitchDefaultsToAuto(t *testing.T) {
	cfg, err := load(writeConfigFile(t, "backend = \"smb\"\n[strm]\nbase_url = \"http://media.example:8080\"\n"),
		lookupEnvironment(map[string]string{
			environmentUsername: "smb-user",
			environmentPassword: "test-only-password",
		}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.SMBSTRMEnabled() {
		t.Fatal("the STRM share is disabled, want the automatic share")
	}
}

// TestSMBSTRMSwitchOnRequiresABaseURL covers the "on" state: it forces the
// share, so the streaming base URL is a requirement rather than a trigger.
func TestSMBSTRMSwitchOnRequiresABaseURL(t *testing.T) {
	_, err := load(writeConfigFile(t, "backend = \"smb\"\n[smb]\nstrm = \"on\"\n"),
		lookupEnvironment(map[string]string{
			environmentUsername: "smb-user",
			environmentPassword: "test-only-password",
		}))
	if err == nil {
		t.Fatal("load accepted the STRM switch \"on\" without a base URL")
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "smb.strm" {
		t.Fatalf("load error = %v, want an smb.strm validation error", err)
	}
}

// TestSMBSTRMSwitchOffNeedsNoBaseURL covers the "off" state: it forbids the
// share, so a missing streaming base URL is not a mistake.
func TestSMBSTRMSwitchOffNeedsNoBaseURL(t *testing.T) {
	cfg, err := load(writeConfigFile(t, "backend = \"smb\"\n[smb]\nstrm = \"off\"\n"),
		lookupEnvironment(map[string]string{
			environmentUsername: "smb-user",
			environmentPassword: "test-only-password",
		}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SMBSTRMEnabled() {
		t.Fatal("the STRM share is enabled, want it forbidden")
	}
}

// TestSMBSTRMSwitchRejectsAnUnknownState covers the enumerated values of the
// switch: the error names the key and the states it accepts.
func TestSMBSTRMSwitchRejectsAnUnknownState(t *testing.T) {
	_, err := load(writeConfigFile(t, "backend = \"smb\"\n[smb]\nstrm = \"maybe\"\n"),
		lookupEnvironment(map[string]string{
			environmentUsername: "smb-user",
			environmentPassword: "test-only-password",
		}))
	if err == nil {
		t.Fatal("load accepted an unknown STRM switch state")
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "smb.strm" {
		t.Fatalf("load error = %v, want an smb.strm validation error", err)
	}
	for _, want := range []string{"auto", "on", "off"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("load error = %q, want it to name the state %q", err, want)
		}
	}
}

// TestSMBSTRMSwitchRejectsAnUnknownStateFromTheEnvironment covers the
// environment variable of the switch: it accepts the same states as the
// configuration file.
func TestSMBSTRMSwitchRejectsAnUnknownStateFromTheEnvironment(t *testing.T) {
	_, err := load(writeConfigFile(t, "backend = \"smb\"\n"),
		lookupEnvironment(map[string]string{
			environmentUsername:  "smb-user",
			environmentPassword:  "test-only-password",
			"TORRENTFS_SMB_STRM": "maybe",
		}))
	if err == nil {
		t.Fatal("load accepted an unknown STRM switch state")
	}
	if !strings.Contains(err.Error(), "smb.strm") {
		t.Fatalf("load error = %q, want the smb.strm key", err)
	}
}

// TestSMBSTRMSwitchIsCheckedWithoutTheSMBBackend covers the strictness rule:
// the value of an enumerated key is always checked, even when the backend it
// belongs to is not selected.
func TestSMBSTRMSwitchIsCheckedWithoutTheSMBBackend(t *testing.T) {
	_, err := load(writeConfigFile(t, "backend = \"none\"\n[smb]\nstrm = \"maybe\"\n"), lookupEnvironment(nil))
	if err == nil {
		t.Fatal("load accepted an unknown STRM switch state")
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "smb.strm" {
		t.Fatalf("load error = %v, want an smb.strm validation error", err)
	}
}

// TestSMBSTRMSwitchIsOverridableByTheEnvironment covers the environment
// variable of the switch: it configures the same key and wins over the
// configuration file.
func TestSMBSTRMSwitchIsOverridableByTheEnvironment(t *testing.T) {
	for _, tt := range []struct {
		state string
		want  bool
	}{
		{state: "on", want: true},
		{state: "off", want: false},
	} {
		t.Run(tt.state, func(t *testing.T) {
			cfg, err := load(writeConfigFile(t, "backend = \"smb\"\n[strm]\nbase_url = \"http://media.example:8080\"\n"+
				"[smb]\nstrm = \"auto\"\n"),
				lookupEnvironment(map[string]string{
					environmentUsername:  "smb-user",
					environmentPassword:  "test-only-password",
					"TORRENTFS_SMB_STRM": tt.state,
				}))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := cfg.SMBSTRMEnabled(); got != tt.want {
				t.Fatalf("the STRM share is enabled = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRemovedSMBSTRMSwitchNamesAreRejectedWithTheirReplacement covers the
// two removed names of the STRM switch.
func TestRemovedSMBSTRMSwitchNamesAreRejectedWithTheirReplacement(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		env     map[string]string
		removed string
		new     string
	}{
		{
			name:    "the old configuration key",
			path:    writeConfigFile(t, "[smb]\nstrm_enabled = true\n"),
			removed: "smb.strm_enabled",
			new:     "smb.strm",
		},
		{
			name:    "the old environment variable",
			env:     map[string]string{"TORRENTFS_SMB_STRM_ENABLED": "true"},
			removed: "TORRENTFS_SMB_STRM_ENABLED",
			new:     "TORRENTFS_SMB_STRM",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(tt.path, lookupEnvironment(tt.env))
			if err == nil {
				t.Fatal("load accepted the removed name")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("load error = %v, want ErrInvalid", err)
			}
			for _, want := range []string{tt.removed, tt.new} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("load error = %q, want it to name %q", err, want)
				}
			}
		})
	}
}
