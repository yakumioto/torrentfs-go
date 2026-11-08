package config

import "testing"

// TestEveryConfigurationKeyDeclaresItsComponent covers the metadata contract
// of the configuration page: every configuration key can be attributed to the
// component that owns it, so the page groups and explains a key without a
// second hand-maintained table.
func TestEveryConfigurationKeyDeclaresItsComponent(t *testing.T) {
	for _, key := range Schema() {
		if key.Component == "" {
			t.Errorf("the configuration key %q declares no component", key.Path)
		}
	}
}

// TestOnlyTheHotApplyBoundaryIsMarkedHotApply covers the hot-apply boundary
// of the first batch: a configuration key is marked hot-applied only when its
// owning component can replace the value in the running process, and every
// other key takes effect at the next start.
func TestOnlyTheHotApplyBoundaryIsMarkedHotApply(t *testing.T) {
	want := map[string]string{
		"log.level":                   "logging",
		"http.max_upload_size":        "api",
		"connections.reading_conns":   "session",
		"connections.uploading_conns": "session",
		"connections.read_grace":      "session",
		"http.auth.token_ttl":         "auth",
	}
	for _, key := range Schema() {
		component, hot := want[key.Path]
		if key.HotApply != hot {
			t.Errorf("the configuration key %q reports HotApply=%t, want %t",
				key.Path, key.HotApply, hot)
		}
		if hot && key.Component != component {
			t.Errorf("the hot-applied configuration key %q belongs to the component %q, want %q",
				key.Path, key.Component, component)
		}
		delete(want, key.Path)
	}
	for path := range want {
		t.Errorf("the schema has no configuration key %q", path)
	}
}
