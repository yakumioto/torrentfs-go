package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestFilesystemPrecedenceLegacyAndCredentials(t *testing.T) {
	file := writeConfigFile(t, "[filesystem]\nmode = 'smb'\n[smb]\nlisten_addr = '127.0.0.1:2445'\nshare_name = 'media'\n")
	cfg, err := load(file, lookupEnvironment(map[string]string{"TORRENTFS_USERNAME": "user", "TORRENTFS_PASSWORD": "secret-value"}))
	if err != nil || cfg.Filesystem.Mode != "smb" || cfg.SMB.ShareName != "media" || cfg.SMB.ListenAddr != "127.0.0.1:2445" {
		t.Fatalf("file config: %v", err)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil || strings.Contains(string(encoded), "secret-value") {
		t.Fatal("serialized SMB secret")
	}
	cfg, err = load(file, lookupEnvironment(map[string]string{"TORRENTFS_SMB_ENABLED": "false"}))
	if err != nil || cfg.Filesystem.Mode != "fuse" {
		t.Fatalf("legacy overrides TOML: %v", err)
	}
	cfg, err = load(file, lookupEnvironment(map[string]string{
		"TORRENTFS_FILESYSTEM_MODE": "fuse", "TORRENTFS_SMB_ENABLED": "invalid",
		"TORRENTFS_SMB_STRM_ENABLED": "false", "TORRENTFS_STRM_ENABLED": "invalid",
		"TORRENTFS_SMB_SHARE_NAME": "override", "TORRENTFS_SMB_LISTEN_ADDR": "127.0.0.1:0",
	}))
	if err != nil || cfg.Filesystem.Mode != "fuse" || cfg.SMB.STRMEnabled || cfg.SMB.ShareName != "override" {
		t.Fatalf("canonical priority: %v", err)
	}
	cfg, err = load("", lookupEnvironment(map[string]string{
		"TORRENTFS_SMB_ENABLED": "true", "TORRENTFS_USERNAME": "user", "TORRENTFS_PASSWORD": "secret",
		"TORRENTFS_STRM_ENABLED": "true", "TORRENTFS_STRM_BASE_URL": "https://media.test/",
	}))
	if err != nil || cfg.Filesystem.Mode != "smb" || !cfg.SMB.STRMEnabled {
		t.Fatalf("legacy aliases: %v", err)
	}
	if cfg.HTTP.Auth.Username != "" || cfg.HTTP.Auth.PasswordHash != "" {
		t.Fatal("SMB credentials changed disabled HTTP authentication")
	}
	cfg, err = load("", lookupEnvironment(map[string]string{
		"TORRENTFS_FILESYSTEM_MODE": "smb", "TORRENTFS_USERNAME": "user", "TORRENTFS_PASSWORD": "secret",
		"TORRENTFS_HTTP_AUTH_ENABLED": "true",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(cfg.HTTP.Auth.PasswordHash), []byte("secret")); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemRejectsInvalidConfiguration(t *testing.T) {
	for _, vars := range []map[string]string{
		{"TORRENTFS_FILESYSTEM_MODE": "other"},
		{"TORRENTFS_FILESYSTEM_MODE": "smb"},
		{"TORRENTFS_FILESYSTEM_MODE": "smb", "TORRENTFS_USERNAME": "u"},
		{"TORRENTFS_FILESYSTEM_MODE": "smb", "TORRENTFS_PASSWORD": "p"},
		{"TORRENTFS_SMB_LISTEN_ADDR": "bad"}, {"TORRENTFS_SMB_LISTEN_ADDR": "127.0.0.1:99999"},
		{"TORRENTFS_SMB_SHARE_NAME": "IPC$"}, {"TORRENTFS_SMB_SHARE_NAME": "a/b"}, {"TORRENTFS_SMB_SHARE_NAME": "a\\b"}, {"TORRENTFS_SMB_SHARE_NAME": "bad\nname"},
		{"TORRENTFS_SMB_ENABLED": "invalid"}, {"TORRENTFS_STRM_ENABLED": "invalid"},
		{"TORRENTFS_FILESYSTEM_MODE": "smb", "TORRENTFS_USERNAME": "u", "TORRENTFS_PASSWORD": "p", "TORRENTFS_SMB_STRM_ENABLED": "true"},
	} {
		if _, err := load("", lookupEnvironment(vars)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid settings were accepted: %v", err)
		}
	}
	file := writeConfigFile(t, "[smb]\npassword = 'must-not-be-in-toml'\n")
	if _, err := load(file, lookupEnvironment(nil)); err == nil {
		t.Fatal("accepted plaintext SMB password in TOML")
	}
}
