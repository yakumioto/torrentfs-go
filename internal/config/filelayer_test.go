package config

import (
	"strings"
	"testing"
)

// TestLoadContentFileLayerSkipsTheRulesThatNeedTheEnvironment covers the
// deployment where the SMB credentials come from the environment: the file
// layer alone cannot satisfy the requirement rules, so reading the layer must
// not apply them. The effective configuration still reports the missing
// requirement when it is loaded without the environment.
func TestLoadContentFileLayerSkipsTheRulesThatNeedTheEnvironment(t *testing.T) {
	body := []byte("backend = \"smb\"\n")

	file, sources, err := LoadContentFileLayer(body, "torrentfs.toml")
	if err != nil {
		t.Fatalf("LoadContentFileLayer: %v", err)
	}
	if file.Backend != BackendSMB {
		t.Errorf("backend = %q, want smb", file.Backend)
	}
	if !sources.FromFile("backend") {
		t.Errorf("the source of backend = %q, want file", sources.SourceOf("backend"))
	}
	if got := sources.SourceOf("username"); got != SourceDefault {
		t.Errorf("the source of username = %q, want default", got)
	}

	if _, err := LoadContentWithEnvironment(body, "torrentfs.toml", emptyEnvironment{}); err == nil {
		t.Fatal("the effective loader accepted an SMB configuration without credentials")
	} else if !strings.Contains(err.Error(), "username") {
		t.Errorf("the effective loader error = %v, want it to name username", err)
	}
}

// TestLoadContentFileLayerRejectsRemovedAndUnknownKeys covers the errors that
// are about the document itself: they must still stop the load.
func TestLoadContentFileLayerRejectsRemovedAndUnknownKeys(t *testing.T) {
	if _, _, err := LoadContentFileLayer([]byte("[lab]\nupload_mode = \"baseline\"\n"), "torrentfs.toml"); err == nil {
		t.Fatal("the file layer accepted a removed configuration section")
	}

	if _, _, err := LoadContentFileLayer([]byte("[nope]\nvalue = 1\n"), "torrentfs.toml"); err == nil {
		t.Fatal("the file layer accepted an unknown configuration section")
	}

	if _, _, err := LoadContentFileLayer([]byte("[log]\nlevel = \"loud\"\n"), "torrentfs.toml"); err != nil {
		t.Fatalf("the file layer rejected a syntactically valid value: %v", err)
	}
}
