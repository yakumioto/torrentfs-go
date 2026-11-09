package configctl_test

import "testing"

// TestViewReadsAFileWhoseCredentialsComeFromTheEnvironment covers the SMB
// deployment whose credentials are environment variables: the configuration
// page must show the file values and the effective values without degrading to
// a file error, even though the file layer alone lacks the credentials.
func TestViewReadsAFileWhoseCredentialsComeFromTheEnvironment(t *testing.T) {
	t.Setenv("TORRENTFS_USERNAME", "torrentfs")
	t.Setenv("TORRENTFS_PASSWORD", "s3cr3t")

	path := writeConfigFile(t, "backend = \"smb\"\n")
	view := loadView(t, path)

	if view.FileError != "" {
		t.Fatalf("file error = %q, want empty", view.FileError)
	}
	if backend := keyByPath(t, view, "backend"); backend.FileValue != "smb" {
		t.Errorf("backend file value = %q, want smb", backend.FileValue)
	}
	username := keyByPath(t, view, "username")
	if username.FileValue != "" {
		t.Errorf("username file value = %q, want the empty default", username.FileValue)
	}
	if username.EffectiveValue != "torrentfs" {
		t.Errorf("username effective value = %q, want torrentfs", username.EffectiveValue)
	}
	if username.Source != "environment" {
		t.Errorf("username source = %q, want environment", username.Source)
	}
}
