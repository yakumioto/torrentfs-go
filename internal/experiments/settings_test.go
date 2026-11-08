package experiments

import (
	"strings"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// TestSettingsFromDefaultsToBaseline covers the contract that the experiment
// is always on: an unconfigured experiment reports the real counters, so the
// session can create a reporter without a configuration switch.
func TestSettingsFromDefaultsToBaseline(t *testing.T) {
	settings, err := SettingsFrom(config.Default().Experiments.Upload)
	if err != nil {
		t.Fatalf("SettingsFrom: %v", err)
	}
	if settings.Mode != ModeBaseline {
		t.Fatalf("mode = %q, want %q", settings.Mode, ModeBaseline)
	}
	if settings.JitterFraction != 0.20 {
		t.Fatalf("default jitter = %v, want 0.20", settings.JitterFraction)
	}
	if settings.CapBytes != 0 {
		t.Fatalf("default cap = %d, want uncapped", settings.CapBytes)
	}
	if settings.Seed == 0 {
		t.Fatal("default seed is zero; expected a wall-clock seed")
	}
	if settings.PeerSampleInterval != defaultPeerSampleInterval {
		t.Fatalf("default sample interval = %v, want %v", settings.PeerSampleInterval, defaultPeerSampleInterval)
	}
	if settings.AnnounceLogPath != "" {
		t.Fatalf("default announce log = %q, want none", settings.AnnounceLogPath)
	}
}

// TestSettingsFromDoesNotInferMode covers the contract that a mode is always
// named: a rate or multiplier alone is a configuration mistake, not an
// inference.
func TestSettingsFromDoesNotInferMode(t *testing.T) {
	cases := []struct {
		name     string
		cfg      config.UploadExperiment
		wantKey  string
		wantHint string
	}{
		{
			name:     "rate without mode",
			cfg:      config.UploadExperiment{Rate: config.ByteRate(2 << 20)},
			wantKey:  "experiments.upload.rate",
			wantHint: `mode = "inject"`,
		},
		{
			name:     "multiplier without mode",
			cfg:      config.UploadExperiment{Multiplier: config.Ratio(5)},
			wantKey:  "experiments.upload.multiplier",
			wantHint: `mode = "amplify"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SettingsFrom(tc.cfg)
			if err == nil {
				t.Fatalf("SettingsFrom(%+v) = nil error, want rejection", tc.cfg)
			}
			for _, want := range []string{tc.wantKey, tc.wantHint} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("SettingsFrom error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// TestSettingsFromBaselineAcceptsQuietKnobs covers the contract that the
// settings that shape the recorded ground truth work in the control arm too.
func TestSettingsFromBaselineAcceptsQuietKnobs(t *testing.T) {
	settings, err := SettingsFrom(config.UploadExperiment{
		Mode:        "baseline",
		Jitter:      config.Percent(10),
		Cap:         config.OptionalByteQuantity(1 << 30),
		Seed:        7,
		AnnounceLog: "/tmp/announces.jsonl",
	})
	if err != nil {
		t.Fatalf("SettingsFrom: %v", err)
	}
	if settings.Mode != ModeBaseline {
		t.Fatalf("mode = %q, want baseline", settings.Mode)
	}
	if settings.JitterFraction != 0.10 {
		t.Fatalf("jitter = %v, want 0.10", settings.JitterFraction)
	}
	if settings.CapBytes != 1<<30 {
		t.Fatalf("cap = %d, want %d", settings.CapBytes, int64(1<<30))
	}
	if settings.Seed != 7 {
		t.Fatalf("seed = %d, want 7", settings.Seed)
	}
}

// TestSettingsFromFullConfiguration covers the contract that every configured
// value reaches the settings.
func TestSettingsFromFullConfiguration(t *testing.T) {
	settings, err := SettingsFrom(config.UploadExperiment{
		Mode:               "inject",
		Rate:               config.ByteRate(2 << 20),
		Jitter:             config.Percent(25),
		Cap:                config.OptionalByteQuantity(3 << 40),
		Seed:               42,
		AnnounceLog:        "/tmp/announces.jsonl",
		PeerSampleInterval: config.OptionalDuration(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("SettingsFrom: %v", err)
	}
	if settings.Mode != ModeInject {
		t.Fatalf("mode = %q, want inject", settings.Mode)
	}
	if want := float64(2 << 20); settings.RateBytesPerSecond != want {
		t.Fatalf("rate = %v, want %v", settings.RateBytesPerSecond, want)
	}
	if settings.JitterFraction != 0.25 {
		t.Fatalf("jitter = %v, want 0.25", settings.JitterFraction)
	}
	if want := int64(3 << 40); settings.CapBytes != want {
		t.Fatalf("cap = %d, want %d", settings.CapBytes, want)
	}
	if settings.Seed != 42 {
		t.Fatalf("seed = %d, want 42", settings.Seed)
	}
	if settings.AnnounceLogPath != "/tmp/announces.jsonl" {
		t.Fatalf("log path = %q", settings.AnnounceLogPath)
	}
	if settings.PeerSampleInterval != 2*time.Second {
		t.Fatalf("sample interval = %v, want 2s", settings.PeerSampleInterval)
	}
}

// TestSettingsFromRejectsInvalidConfiguration covers the contract that a
// partially configured setup is rejected instead of silently changing what
// the tracker sees.
func TestSettingsFromRejectsInvalidConfiguration(t *testing.T) {
	second := config.OptionalDuration(time.Second)
	cases := map[string]config.UploadExperiment{
		"unknown mode":                {Mode: "turbo"},
		"inject without rate":         {Mode: "inject"},
		"amplify without factor":      {Mode: "amplify"},
		"baseline with rate":          {Mode: "baseline", Rate: config.ByteRate(1 << 20)},
		"baseline with multiplier":    {Mode: "baseline", Multiplier: config.Ratio(2)},
		"inject with multiplier":      {Mode: "inject", Rate: config.ByteRate(1 << 20), Multiplier: config.Ratio(2)},
		"amplify with rate":           {Mode: "amplify", Multiplier: config.Ratio(2), Rate: config.ByteRate(1 << 20)},
		"amplify with multiplier one": {Mode: "amplify", Multiplier: config.Ratio(1)},
		"interval without log":        {Mode: "baseline", PeerSampleInterval: second},
		"inject interval without log": {Mode: "inject", Rate: config.ByteRate(1 << 20), PeerSampleInterval: second},
		"jitter above one hundred":    {Mode: "inject", Rate: config.ByteRate(1 << 20), Jitter: config.Percent(101)},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := SettingsFrom(cfg); err == nil {
				t.Fatalf("SettingsFrom(%+v) = nil error, want rejection", cfg)
			}
		})
	}
}

// TestNormalizeRuntimeSettings covers the contract that the runtime settings
// are consistent with the mode before they reach the reporter.
func TestNormalizeRuntimeSettings(t *testing.T) {
	cases := []struct {
		name    string
		input   RuntimeSettings
		want    RuntimeSettings
		wantErr bool
	}{
		{
			name:  "baseline clears unused knobs",
			input: RuntimeSettings{Mode: ModeBaseline, RateBytesPerSecond: 10, Multiplier: 5, JitterFraction: 0.2, CapBytes: 100},
			want:  RuntimeSettings{Mode: ModeBaseline, JitterFraction: 0.2, CapBytes: 100},
		},
		{
			name:  "inject keeps the rate",
			input: RuntimeSettings{Mode: ModeInject, RateBytesPerSecond: 1 << 20, Multiplier: 5, JitterFraction: 0.2},
			want:  RuntimeSettings{Mode: ModeInject, RateBytesPerSecond: 1 << 20, JitterFraction: 0.2},
		},
		{
			name:  "amplify keeps the multiplier",
			input: RuntimeSettings{Mode: ModeAmplify, RateBytesPerSecond: 10, Multiplier: 5, JitterFraction: 0.2, CapBytes: 5},
			want:  RuntimeSettings{Mode: ModeAmplify, Multiplier: 5, JitterFraction: 0.2, CapBytes: 5},
		},
		{name: "inject without rate", input: RuntimeSettings{Mode: ModeInject, JitterFraction: 0.2}, wantErr: true},
		{name: "amplify without multiplier", input: RuntimeSettings{Mode: ModeAmplify, JitterFraction: 0.2}, wantErr: true},
		{name: "amplify multiplier at one", input: RuntimeSettings{Mode: ModeAmplify, Multiplier: 1, JitterFraction: 0.2}, wantErr: true},
		{name: "unknown mode", input: RuntimeSettings{Mode: "turbo"}, wantErr: true},
		{name: "jitter above one", input: RuntimeSettings{Mode: ModeBaseline, JitterFraction: 1.1}, wantErr: true},
		{name: "negative jitter", input: RuntimeSettings{Mode: ModeBaseline, JitterFraction: -0.1}, wantErr: true},
		{name: "negative cap", input: RuntimeSettings{Mode: ModeBaseline, JitterFraction: 0.2, CapBytes: -1}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeRuntimeSettings(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NormalizeRuntimeSettings(%+v) = nil error, want rejection", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeRuntimeSettings(%+v): %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeRuntimeSettings(%+v) = %+v, want %+v", tc.input, got, tc.want)
			}
		})
	}
}

// TestSettingsFromNamesTheNewConfigurationKeys covers the contract that a
// rejection names the configuration keys the user now writes, so the message
// matches the documentation.
func TestSettingsFromNamesTheNewConfigurationKeys(t *testing.T) {
	cases := map[string]struct {
		cfg  config.UploadExperiment
		want []string
	}{
		"unknown mode": {
			cfg:  config.UploadExperiment{Mode: "turbo"},
			want: []string{"experiments.upload.mode"},
		},
		"rate without mode": {
			cfg:  config.UploadExperiment{Rate: config.ByteRate(1 << 20)},
			want: []string{"experiments.upload.rate", "experiments.upload.mode", `mode = "inject"`},
		},
		"interval without log": {
			cfg:  config.UploadExperiment{Mode: "baseline", PeerSampleInterval: config.OptionalDuration(time.Second)},
			want: []string{"experiments.upload.peer_sample_interval", "experiments.upload.announce_log"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := SettingsFrom(tc.cfg)
			if err == nil {
				t.Fatal("SettingsFrom accepted an invalid configuration")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("SettingsFrom error %q does not name %q", err, want)
				}
			}
		})
	}
}
