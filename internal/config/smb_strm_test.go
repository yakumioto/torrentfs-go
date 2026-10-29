package config

import "testing"

func TestSMBSTRMShareSelection(t *testing.T) {
	for _, tc := range []struct {
		name, setting, base string
		want, invalid       bool
	}{
		{"no URL", "", "", false, false},
		{"automatic", "", "http://media.example.test:8080", true, false},
		{"disabled", "strm_enabled=false", "http://media.example.test:8080", false, false},
		{"enabled", "strm_enabled=true", "http://media.example.test:8080", true, false},
		{"requires URL", "strm_enabled=true", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := "[filesystem]\nbackend=\"smb\"\n[strm]\nbase_url=\"" + tc.base + "\"\n[smb]\n" + tc.setting
			cfg, err := loadFilesystemTest(t, text, map[string]string{
				"TORRENTFS_USERNAME": "user", "TORRENTFS_PASSWORD": "test-only-password",
			})
			if tc.invalid {
				if err == nil {
					t.Fatal("missing STRM URL accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.SMBSTRMEnabled(); got != tc.want {
				t.Fatalf("enabled=%v want %v", got, tc.want)
			}
		})
	}
}

func TestLegacySTRMSwitch(t *testing.T) {
	for _, enabled := range []string{"true", "false"} {
		cfg, err := loadFilesystemTest(t, "[strm]\nbase_url=\"http://media.example.test:8080\"", map[string]string{
			"TORRENTFS_SMB_ENABLED": "true", "TORRENTFS_USERNAME": "user",
			"TORRENTFS_PASSWORD": "test-only-password", "TORRENTFS_STRM_ENABLED": enabled,
		})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.SMBSTRMEnabled() != (enabled == "true") {
			t.Fatalf("legacy switch %s ignored", enabled)
		}
	}
	if _, err := loadFilesystemTest(t, "", map[string]string{"TORRENTFS_STRM_ENABLED": "true"}); err == nil {
		t.Fatal("legacy STRM switch accepted without SMB")
	}
	if _, err := loadFilesystemTest(t, "", map[string]string{
		"TORRENTFS_SMB_ENABLED": "true", "TORRENTFS_USERNAME": "user", "TORRENTFS_PASSWORD": "test-only-password",
		"TORRENTFS_STRM_ENABLED": "true", "TORRENTFS_SMB_STRM_ENABLED": "false",
	}); err == nil {
		t.Fatal("conflicting STRM switches accepted")
	}
}
