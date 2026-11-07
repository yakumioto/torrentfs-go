package config

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// TestCredentialsComeFromTheConfigurationFile covers the top-level
// credentials: a configuration file supplies them, and they land in the
// effective configuration.
func TestCredentialsComeFromTheConfigurationFile(t *testing.T) {
	cfg, err := load(writeConfigFile(t, "backend = \"smb\"\nusername = \"reader\"\npassword = \"a-file-password\"\n"),
		lookupEnvironment(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Username != "reader" {
		t.Fatalf("username = %q, want %q", cfg.Username, "reader")
	}
	if cfg.Password != "a-file-password" {
		t.Fatalf("password = %q, want the configured password", cfg.Password)
	}
}

// TestCredentialsComeFromTheEnvironment covers the same credentials supplied
// by the environment: the derived variable names configure them.
func TestCredentialsComeFromTheEnvironment(t *testing.T) {
	cfg, err := load("", lookupEnvironment(map[string]string{
		"TORRENTFS_USERNAME": "reader",
		"TORRENTFS_PASSWORD": "an-environment-password",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Username != "reader" {
		t.Fatalf("username = %q, want %q", cfg.Username, "reader")
	}
	if cfg.Password != "an-environment-password" {
		t.Fatalf("password = %q, want the environment password", cfg.Password)
	}
}

// TestCredentialsHaveNoDefault covers the promise that the credentials carry
// no default: a deployment that does not configure them starts without them.
func TestCredentialsHaveNoDefault(t *testing.T) {
	cfg := Default()
	if cfg.Username != "" || cfg.Password != "" {
		t.Fatalf("default credentials = %q/%q, want both empty", cfg.Username, cfg.Password)
	}
	for _, path := range []string{"username", "password"} {
		value, ok := SchemaDefault(path)
		if !ok {
			t.Fatalf("the schema has no configuration key %q", path)
		}
		if value != "" {
			t.Fatalf("the default of %q = %v, want an empty value", path, value)
		}
	}
}

// TestCredentialsAreOverriddenByTheEnvironment covers the precedence: the
// environment wins over the configuration file.
func TestCredentialsAreOverriddenByTheEnvironment(t *testing.T) {
	cfg, err := load(writeConfigFile(t, "backend = \"smb\"\nusername = \"file-user\"\npassword = \"file-password\"\n"),
		lookupEnvironment(map[string]string{
			"TORRENTFS_USERNAME": "env-user",
			"TORRENTFS_PASSWORD": "env-password",
		}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Username != "env-user" || cfg.Password != "env-password" {
		t.Fatalf("credentials = %q/%q, want the environment values", cfg.Username, cfg.Password)
	}
}

// TestCredentialRequirements covers the completeness rules: the password is
// required for the SMB output and for enabled HTTP authentication, and the
// user name is required for the SMB output only.
func TestCredentialRequirements(t *testing.T) {
	tests := []struct {
		name         string
		backend      Backend
		authEnabled  bool
		username     string
		password     string
		wantUsername string
		wantPassword string
		wantError    bool
	}{
		{
			name:         "no output and no authentication needs no credentials",
			backend:      BackendNone,
			wantUsername: "",
			wantPassword: "",
		},
		{
			name:         "SMB needs both credentials",
			backend:      BackendSMB,
			username:     "reader",
			password:     "a-password",
			wantUsername: "reader",
			wantPassword: "a-password",
		},
		{
			name:      "SMB without a user name is rejected",
			backend:   BackendSMB,
			password:  "a-password",
			wantError: true,
		},
		{
			name:      "SMB without a password is rejected",
			backend:   BackendSMB,
			username:  "reader",
			wantError: true,
		},
		{
			name:         "the FUSE output needs no credentials",
			backend:      BackendFUSE,
			wantUsername: "",
			wantPassword: "",
		},
		{
			name:         "HTTP authentication needs a password",
			backend:      BackendNone,
			authEnabled:  true,
			password:     "a-password",
			wantUsername: "",
			wantPassword: "a-password",
		},
		{
			name:        "HTTP authentication without a password is rejected",
			backend:     BackendNone,
			authEnabled: true,
			wantError:   true,
		},
		{
			name:         "HTTP authentication without a user name is accepted",
			backend:      BackendNone,
			authEnabled:  true,
			password:     "a-password",
			wantUsername: "",
			wantPassword: "a-password",
		},
		{
			name:         "disabled HTTP authentication needs no password",
			backend:      BackendNone,
			wantUsername: "",
			wantPassword: "",
		},
		{
			name:         "SMB with HTTP authentication needs one password",
			backend:      BackendSMB,
			authEnabled:  true,
			username:     "reader",
			password:     "a-password",
			wantUsername: "reader",
			wantPassword: "a-password",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The credentials are top-level keys, so they have to precede
			// every section header: in TOML a bare key after a table
			// header belongs to that table.
			body := "backend = \"" + string(tt.backend) + "\"\n"
			if tt.username != "" {
				body += "username = \"" + tt.username + "\"\n"
			}
			if tt.password != "" {
				body += "password = \"" + tt.password + "\"\n"
			}
			if tt.authEnabled {
				body += "[http.auth]\nenabled = true\n"
			}
			if tt.backend == BackendFUSE {
				body += "[fuse]\nmountpoint = \"/virtual-mount\"\n"
			}
			cfg, err := load(writeConfigFile(t, body), lookupEnvironment(nil))
			if tt.wantError {
				if err == nil {
					t.Fatal("load accepted an incomplete credential configuration")
				}
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("load error = %v, want ErrInvalid", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.Username != tt.wantUsername || cfg.Password != tt.wantPassword {
				t.Fatalf("credentials = %q/%q, want %q/%q",
					cfg.Username, cfg.Password, tt.wantUsername, tt.wantPassword)
			}
		})
	}
}

// TestCredentialRequirementsNameTheMissingKey covers the error contract: the
// error names the configuration key that has to be supplied.
func TestCredentialRequirementsNameTheMissingKey(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		field string
	}{
		{
			name:  "SMB without a user name",
			body:  "backend = \"smb\"\npassword = \"a-password\"\n",
			field: "username",
		},
		{
			name:  "SMB without a password",
			body:  "backend = \"smb\"\nusername = \"reader\"\n",
			field: "password",
		},
		{
			name:  "HTTP authentication without a password",
			body:  "[http.auth]\nenabled = true\n",
			field: "password",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(writeConfigFile(t, tt.body), lookupEnvironment(nil))
			if err == nil {
				t.Fatal("load accepted an incomplete credential configuration")
			}
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("load error = %T %v, want a ValidationError", err, err)
			}
			if validationErr.Field != tt.field {
				t.Fatalf("ValidationError.Field = %q, want %q", validationErr.Field, tt.field)
			}
		})
	}
}

// TestCredentialsAreNeverDisclosedInErrors covers the secrecy requirement: no
// error message names the password value.
func TestCredentialsAreNeverDisclosedInErrors(t *testing.T) {
	const secret = "must-not-appear-in-errors"
	for _, body := range []string{
		// A password with a newline is rejected.
		"backend = \"smb\"\nusername = \"reader\"\npassword = \"" + secret + "\n",
		// A password longer than bcrypt accepts is rejected.
		"[http.auth]\nenabled = true\npassword = \"" + secret + strings.Repeat("x", 80) + "\"\n",
	} {
		_, err := load(writeConfigFile(t, body), lookupEnvironment(nil))
		if err == nil {
			t.Fatalf("load accepted the invalid configuration %q", body)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("load error = %q, must not contain the password", err)
		}
	}
	for _, environment := range []map[string]string{
		{"TORRENTFS_BACKEND": "smb", "TORRENTFS_USERNAME": "reader", "TORRENTFS_PASSWORD": secret + "\n"},
		{"TORRENTFS_HTTP_AUTH_ENABLED": "true", "TORRENTFS_PASSWORD": secret + strings.Repeat("x", 80)},
	} {
		_, err := load("", lookupEnvironment(environment))
		if err == nil {
			t.Fatal("load accepted the invalid environment")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("load error = %q, must not contain the password", err)
		}
	}
}

// TestPasswordLengthLimit covers the bcrypt limit: a password longer than
// bcrypt accepts is rejected instead of silently truncated.
func TestPasswordLengthLimit(t *testing.T) {
	for _, password := range []string{strings.Repeat("x", 72), strings.Repeat("密", 24)} {
		cfg, err := load("", lookupEnvironment(map[string]string{
			"TORRENTFS_HTTP_AUTH_ENABLED": "true",
			"TORRENTFS_PASSWORD":          password,
		}))
		if err != nil {
			t.Fatalf("load a password of %d bytes: %v", len(password), err)
		}
		if cfg.Password != password {
			t.Fatalf("the password of %d bytes was changed", len(password))
		}
	}
	for _, password := range []string{strings.Repeat("x", 73), strings.Repeat("密", 25)} {
		_, err := load("", lookupEnvironment(map[string]string{
			"TORRENTFS_HTTP_AUTH_ENABLED": "true",
			"TORRENTFS_PASSWORD":          password,
		}))
		if err == nil {
			t.Fatalf("load accepted a password of %d bytes", len(password))
		}
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("load error = %v, want ErrInvalid", err)
		}
	}
}

// TestPasswordHash covers the hash the HTTP login verifies: the hash of the
// configured password matches the password.
func TestPasswordHash(t *testing.T) {
	cfg := Default()
	cfg.Password = "a-password"
	hash, err := cfg.PasswordHash()
	if err != nil {
		t.Fatalf("PasswordHash: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword(hash, []byte("a-password")); err != nil {
		t.Fatalf("the hash does not match the password: %v", err)
	}
	if cost, err := bcrypt.Cost(hash); err != nil || cost != bcrypt.DefaultCost {
		t.Fatalf("hash cost = %d, error = %v, want %d", cost, err, bcrypt.DefaultCost)
	}
}

// TestPasswordHashIsEmptyWithoutAPassword covers the deployment without
// credentials: there is nothing to hash.
func TestPasswordHashIsEmptyWithoutAPassword(t *testing.T) {
	hash, err := Default().PasswordHash()
	if err != nil {
		t.Fatalf("PasswordHash: %v", err)
	}
	if len(hash) != 0 {
		t.Fatalf("hash = %q, want no hash", hash)
	}
}

// TestTheConfigurationLoadsNoSecretFile covers the removal of the secret
// files: the configuration no longer knows a key that names one, so a
// document that points at a file cannot make the loader read it.
func TestTheConfigurationLoadsNoSecretFile(t *testing.T) {
	for _, path := range []string{
		"http.auth.password_hash_file",
		"smb.password_file",
	} {
		for _, key := range Schema() {
			if key.Path == path {
				t.Fatalf("the schema still knows the secret file key %q", path)
			}
		}
	}
	// A document that names a file is rejected outright, so the loader
	// never reaches the file.
	_, err := load(writeConfigFile(t, "backend = \"smb\"\nusername = \"reader\"\n"+
		"[smb]\npassword_file = \"/does/not/exist\"\n"), lookupEnvironment(nil))
	if err == nil {
		t.Fatal("load accepted the removed secret file key")
	}
	if !strings.Contains(err.Error(), "smb.password_file") {
		t.Fatalf("load error = %q, want it to name the removed key", err)
	}
}

// TestSecretConfigurationKeysAreMarked covers the secret marker: the schema
// knows that the password must not be disclosed.
func TestSecretConfigurationKeysAreMarked(t *testing.T) {
	secret := map[string]bool{}
	for _, key := range Schema() {
		if key.Secret {
			secret[key.Path] = true
		}
	}
	if !secret["password"] {
		t.Fatal("the password is not marked as a secret")
	}
	if secret["username"] {
		t.Fatal("the user name is marked as a secret")
	}
	if !SchemaSecret("password") {
		t.Fatal("SchemaSecret does not report the password as a secret")
	}
	if SchemaSecret("username") {
		t.Fatal("SchemaSecret reports the user name as a secret")
	}
	if SchemaSecret("does.not.exist") {
		t.Fatal("SchemaSecret reports an unknown path as a secret")
	}
}
