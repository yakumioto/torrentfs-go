package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// TestValidateReportsEveryInvalidValue covers the contract that a user who
// changed several values learns about all of them in one run: the validation
// reports every invalid configuration key, not just the first one.
func TestValidateReportsEveryInvalidValue(t *testing.T) {
	cfg := config.Default()
	cfg.Cache.CapacityBytes = 0
	cfg.Proxy.Socks5URL = "http://proxy.example"
	cfg.Log.Level = "nonsense"
	cfg.Connections.ListenPort = 70000

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate succeeded")
	}
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("Validate error = %v, want config.ErrInvalid", err)
	}

	var validationErrs *config.ValidationErrors
	if !errors.As(err, &validationErrs) {
		t.Fatalf("Validate error = %T, want *config.ValidationErrors", err)
	}
	if len(validationErrs.Errors) < 4 {
		t.Fatalf("Validate reported %d errors, want at least 4: %v", len(validationErrs.Errors), err)
	}

	wantFields := []string{"cache.capacity", "proxy.socks5_url", "log.level", "connections.listen_port"}
	for _, field := range wantFields {
		if !validationErrs.HasField(field) {
			t.Errorf("Validate did not report %q: %v", field, err)
		}
	}
}

// TestValidateErrorNamesEveryConfigurationKey covers the contract that each
// reported error names the configuration key it belongs to.
func TestValidateErrorNamesEveryConfigurationKey(t *testing.T) {
	cfg := config.Default()
	cfg.Cache.CapacityBytes = 0
	cfg.Log.Level = "nonsense"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate succeeded")
	}
	for _, field := range []string{"cache.capacity", "log.level"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("Validate error %q does not name %q", err, field)
		}
	}
}

// TestValidateAcceptsValidConfiguration covers the contract that a valid
// configuration passes the validation.
func TestValidateAcceptsValidConfiguration(t *testing.T) {
	if err := config.Default().Validate(); err != nil {
		t.Fatalf("Validate default: %v", err)
	}
}

// TestValidateSyntaxIgnoresSelectedBackend covers the contract that a value
// that can never work is rejected wherever it stands: the settings of an
// output that is not selected are still checked for their shape.
func TestValidateSyntaxIgnoresSelectedBackend(t *testing.T) {
	tests := []struct {
		name  string
		setup func(cfg *config.Config)
		field string
	}{
		{
			name:  "an invalid SMB listen address",
			setup: func(cfg *config.Config) { cfg.SMB.ListenAddr = "not-a-host-port" },
			field: "smb.listen_addr",
		},
		{
			name:  "an out-of-range SMB port",
			setup: func(cfg *config.Config) { cfg.SMB.ListenAddr = "0.0.0.0:70000" },
			field: "smb.listen_addr",
		},
		{
			name: "a user name with a control character",
			setup: func(cfg *config.Config) {
				cfg.Username = "reader\x00name"
			},
			field: "username",
		},
		{
			name: "a password with a line break",
			setup: func(cfg *config.Config) {
				cfg.Password = "two\nlines"
			},
			field: "password",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Backend = config.BackendFUSE
			cfg.FUSE.Mountpoint = "/mnt"
			tt.setup(&cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate succeeded")
			}
			var validationErrs *config.ValidationErrors
			if !errors.As(err, &validationErrs) {
				t.Fatalf("Validate error = %T, want *config.ValidationErrors", err)
			}
			if !validationErrs.HasField(tt.field) {
				t.Fatalf("Validate did not report %q: %v", tt.field, err)
			}
		})
	}
}

// TestValidateRequirementsFollowSelectedBackend covers the contract that a
// user can keep the settings of two outputs and switch between them by
// changing one value: the requirements of an output that is not selected are
// not enforced.
func TestValidateRequirementsFollowSelectedBackend(t *testing.T) {
	cfg := config.Default()
	cfg.Backend = config.BackendFUSE
	cfg.FUSE.Mountpoint = "/mnt"
	cfg.SMB.ListenAddr = "0.0.0.0:1445"

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate with an unselected SMB output: %v", err)
	}

	cfg.Backend = config.BackendSMB
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate succeeded without SMB credentials")
	}
	var validationErrs *config.ValidationErrors
	if !errors.As(err, &validationErrs) {
		t.Fatalf("Validate error = %T, want *config.ValidationErrors", err)
	}
	if !validationErrs.HasField("username") {
		t.Errorf("Validate did not report the missing user name: %v", err)
	}
	if !validationErrs.HasField("password") {
		t.Errorf("Validate did not report the missing password: %v", err)
	}
}

// TestValidateDoesNotLeakThePassword covers the contract that a password never
// reaches an error message.
func TestValidateDoesNotLeakThePassword(t *testing.T) {
	const secret = "a-password-that-must-not-leak"
	cfg := config.Default()
	cfg.Backend = config.BackendSMB
	cfg.Username = "reader"
	cfg.Password = secret + "\n"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Validate error disclosed the password: %v", err)
	}
}

// TestLoadReportsEveryInvalidValue covers the contract that a configuration
// file with several mistakes reports all of them when it is loaded.
func TestLoadReportsEveryInvalidValue(t *testing.T) {
	path := writeConfig(t, "backend = \"fuse\"\n[cache]\ncapacity = \"0B\"\n[log]\nlevel = \"nonsense\"\n")

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load succeeded")
	}
	for _, field := range []string{"cache.capacity", "log.level", "fuse.mountpoint"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("Load error %q does not name %q", err, field)
		}
	}
}

// TestLoadReportsEveryInvalidEnvironmentValue covers the contract that
// several malformed environment variables are reported together.
func TestLoadReportsEveryInvalidEnvironmentValue(t *testing.T) {
	lookup := testEnvironment(map[string]string{
		"TORRENTFS_CONNECTIONS_LISTEN_PORT": "not-a-port",
		"TORRENTFS_LOG_LEVEL":               "nonsense",
		"TORRENTFS_CACHE_CAPACITY":          "not-a-quantity",
	})

	_, err := config.LoadWithEnvironment("", lookup)
	if err == nil {
		t.Fatal("Load succeeded")
	}
	for _, field := range []string{"connections.listen_port", "log.level", "cache.capacity"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("Load error %q does not name %q", err, field)
		}
	}
}

// TestValidateOutputRequiresAnOutput covers the contract that the process
// needs something to serve: a configuration that enables no output is
// reported.
func TestValidateOutputRequiresAnOutput(t *testing.T) {
	cfg := config.Default()
	cfg.Backend = config.BackendNone
	cfg.HTTP.ListenAddr = ""

	err := cfg.ValidateOutput()
	if err == nil {
		t.Fatal("ValidateOutput succeeded")
	}
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("ValidateOutput error = %v, want config.ErrInvalid", err)
	}

	cfg.HTTP.ListenAddr = "127.0.0.1:8080"
	if err := cfg.ValidateOutput(); err != nil {
		t.Fatalf("ValidateOutput with the HTTP output: %v", err)
	}
}

// TestValidateOutputIsNotPartOfValidate covers the contract that a
// configuration file may describe a service that is switched off: the
// requirement of an enabled output belongs to the process, not to the values.
func TestValidateOutputIsNotPartOfValidate(t *testing.T) {
	cfg := config.Default()
	cfg.Backend = config.BackendNone
	cfg.HTTP.ListenAddr = ""

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate a configuration that enables nothing: %v", err)
	}
}

// TestValidationErrorsUnwrap covers the contract that the aggregated errors
// can be inspected one by one.
func TestValidationErrorsUnwrap(t *testing.T) {
	cfg := config.Default()
	cfg.Cache.CapacityBytes = 0
	cfg.Log.Level = "nonsense"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate succeeded")
	}
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("Validate error = %T, want *config.ValidationError", err)
	}
	if validationErr.Field == "" {
		t.Error("the unwrapped error names no configuration key")
	}
}

// TestValidateDoesNotLeakAPasswordFromTheEnvironment covers the contract that
// a malformed environment value never discloses the value it holds.
func TestValidateDoesNotLeakAPasswordFromTheEnvironment(t *testing.T) {
	const secret = "a-password-that-must-not-leak"
	lookup := testEnvironment(map[string]string{
		"TORRENTFS_PASSWORD": secret + "\n",
	})

	_, err := config.LoadWithEnvironment("", lookup)
	if err == nil {
		t.Fatal("Load succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Load error disclosed the password: %v", err)
	}
}

// TestLoadReportsEveryRemovedName covers the contract that every removed name
// is reported, not just the first one.
func TestLoadReportsEveryRemovedName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "torrentfs.toml")
	if err := os.WriteFile(path, []byte("[mount]\nread_timeout = \"30s\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	lookup := testEnvironment(map[string]string{"TORRENTFS_MOUNT_ALLOW_OTHER": "true"})

	_, err := config.LoadWithEnvironment(path, lookup)
	if err == nil {
		t.Fatal("Load succeeded")
	}
	if !strings.Contains(err.Error(), "mount.read_timeout") {
		t.Errorf("Load error %q does not name the removed TOML key", err)
	}
	if !strings.Contains(err.Error(), "TORRENTFS_MOUNT_ALLOW_OTHER") {
		t.Errorf("Load error %q does not name the removed environment variable", err)
	}
}
