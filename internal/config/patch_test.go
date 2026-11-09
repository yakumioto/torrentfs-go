package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestSetTextAcceptsTheTextFormOfEveryKind covers the parsing contract of the
// configuration page: a value the page submits as text is parsed with the same
// rules as the environment variable of the key, so the two inputs accept the
// same values.
func TestSetTextAcceptsTheTextFormOfEveryKind(t *testing.T) {
	cases := []struct {
		path string
		text string
		want func(Config) bool
	}{
		{"backend", "smb", func(c Config) bool { return c.Backend == BackendSMB }},
		{"log.level", "debug", func(c Config) bool { return c.Log.Level == "debug" }},
		{"log.add_source", "true", func(c Config) bool { return c.Log.AddSource }},
		{"connections.listen_port", "51413", func(c Config) bool { return c.Connections.ListenPort == 51413 }},
		{"connections.read_grace", "90s", func(c Config) bool { return c.Connections.ReadGrace == Duration(90*time.Second) }},
		{"connections.bootstrap_nodes", "a.example:1, b.example:2", func(c Config) bool {
			return strings.Join(c.Connections.BootstrapNodes, ",") == "a.example:1,b.example:2"
		}},
		{"cache.capacity", "512MiB", func(c Config) bool { return c.Cache.CapacityBytes == 512<<20 }},
		{"http.max_upload_size", "32MB", func(c Config) bool { return c.HTTP.MaxUploadBytes == 32_000_000 }},
		{"connections.listen_port", "0", func(c Config) bool { return c.Connections.ListenPort == 0 }},
		{"experiments.upload.rate", "1MiB/s", func(c Config) bool { return c.Experiments.Upload.Rate == 1<<20 }},
		{"experiments.upload.rate", "", func(c Config) bool { return c.Experiments.Upload.Rate == 0 }},
		{"experiments.upload.multiplier", "2.5", func(c Config) bool { return c.Experiments.Upload.Multiplier == Ratio(2.5) }},
		{"experiments.upload.multiplier", "", func(c Config) bool { return c.Experiments.Upload.Multiplier == 0 }},
		{"experiments.upload.jitter", "12", func(c Config) bool { return c.Experiments.Upload.Jitter == Percent(12) }},
		{"experiments.upload.jitter", "", func(c Config) bool { return c.Experiments.Upload.Jitter == 0 }},
		{"experiments.upload.seed", "42", func(c Config) bool { return c.Experiments.Upload.Seed == 42 }},
		{"experiments.upload.cap", "", func(c Config) bool { return c.Experiments.Upload.Cap == 0 }},
		{"experiments.upload.peer_sample_interval", "", func(c Config) bool { return c.Experiments.Upload.PeerSampleInterval == 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			cfg := Default()
			if err := SetText(&cfg, tc.path, tc.text); err != nil {
				t.Fatalf("SetText(%q, %q): %v", tc.path, tc.text, err)
			}
			if !tc.want(cfg) {
				t.Fatalf("SetText(%q, %q) left the value unset", tc.path, tc.text)
			}
		})
	}
}

// TestSetTextReportsAnUnparsableValue covers the contract that a value the page
// submits is reported against its configuration key, so the page can show the
// problem next to the field.
func TestSetTextReportsAnUnparsableValue(t *testing.T) {
	cfg := Default()
	err := SetText(&cfg, "cache.capacity", "not-a-size")
	if err == nil {
		t.Fatal("SetText accepted an unparsable byte quantity")
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("SetText error %v is not a ValidationError", err)
	}
	if validationErr.Field != "cache.capacity" {
		t.Errorf("the error names %q, want cache.capacity", validationErr.Field)
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("the error does not report ErrInvalid: %v", err)
	}
}

// TestSetTextRejectsAnUnknownKey covers the contract that the page cannot write
// a key the schema does not know.
func TestSetTextRejectsAnUnknownKey(t *testing.T) {
	cfg := Default()
	if err := SetText(&cfg, "cache.capacity_bytes", "1GiB"); err == nil {
		t.Fatal("SetText accepted an unknown configuration key")
	}
}

// TestSetTextRefusesASecret covers the boundary of the configuration page: a
// secret key is never edited from text, because the page never receives its
// value.
func TestSetTextRefusesASecret(t *testing.T) {
	cfg := Default()
	if err := SetText(&cfg, "password", "a-new-password"); err == nil {
		t.Fatal("SetText accepted a secret configuration key")
	}
}

// TestMergeKeysCopiesOnlyTheNamedValues covers the hot-apply boundary: the
// running configuration adopts the values of the keys that took effect and
// keeps the previous values of the keys that wait for a restart.
func TestMergeKeysCopiesOnlyTheNamedValues(t *testing.T) {
	src := Default()
	src.Log.Level = "debug"
	src.Cache.CapacityBytes = 4 << 30
	src.Connections.BootstrapNodes = []string{"a.example:1"}

	dst := Default()
	dst.Log.Level = "info"
	dst.Cache.CapacityBytes = 1 << 30
	dst.Connections.BootstrapNodes = []string{"old.example:1"}

	if err := MergeKeys(&dst, src, []string{"log.level"}); err != nil {
		t.Fatalf("MergeKeys: %v", err)
	}
	if dst.Log.Level != "debug" {
		t.Errorf("log.level = %q, want the merged value debug", dst.Log.Level)
	}
	if dst.Cache.CapacityBytes != 1<<30 {
		t.Errorf("cache.capacity = %d, want the untouched value %d", dst.Cache.CapacityBytes, int64(1<<30))
	}
	if strings.Join(dst.Connections.BootstrapNodes, ",") != "old.example:1" {
		t.Errorf("bootstrap nodes = %v, want the untouched value", dst.Connections.BootstrapNodes)
	}

	if err := MergeKeys(&dst, src, []string{"connections.bootstrap_nodes"}); err != nil {
		t.Fatalf("MergeKeys: %v", err)
	}
	if strings.Join(dst.Connections.BootstrapNodes, ",") != "a.example:1" {
		t.Errorf("bootstrap nodes = %v, want the merged value", dst.Connections.BootstrapNodes)
	}
	dst.Connections.BootstrapNodes[0] = "changed.example:1"
	if src.Connections.BootstrapNodes[0] != "a.example:1" {
		t.Error("MergeKeys aliased the source slice; changing the destination changed the source")
	}

	if err := MergeKeys(&dst, src, []string{"no.such.key"}); err == nil {
		t.Fatal("MergeKeys accepted an unknown configuration key")
	}
}
