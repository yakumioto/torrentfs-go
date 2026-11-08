package experiments

import (
	"strings"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// TestSettingsFromDisabled covers the contract that an unconfigured
// experiment is off and changes nothing.
func TestSettingsFromDisabled(t *testing.T) {
	settings, enabled, err := SettingsFrom(config.UploadExperiment{})
	if err != nil {
		t.Fatalf("SettingsFrom: %v", err)
	}
	if enabled {
		t.Fatal("experiment is enabled without any configuration")
	}
	if settings != (Settings{}) {
		t.Fatalf("settings = %+v, want the zero value", settings)
	}
}

// TestSettingsFromInfersMode covers the contract that an experiment mode is
// inferred from the settings when it is not named.
func TestSettingsFromInfersMode(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.UploadExperiment
		want Mode
	}{
		{name: "rate implies inject", cfg: config.UploadExperiment{Rate: config.ByteRate(2 << 20)}, want: ModeInject},
		{name: "multiplier implies amplify", cfg: config.UploadExperiment{Multiplier: config.Ratio(5)}, want: ModeAmplify},
		{name: "log implies baseline", cfg: config.UploadExperiment{AnnounceLog: "announces.jsonl"}, want: ModeBaseline},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings, enabled, err := SettingsFrom(tc.cfg)
			if err != nil {
				t.Fatalf("SettingsFrom: %v", err)
			}
			if !enabled {
				t.Fatal("experiment is disabled although it is configured")
			}
			if settings.Mode != tc.want {
				t.Fatalf("mode = %q, want %q", settings.Mode, tc.want)
			}
		})
	}
}

// TestSettingsFromFullConfiguration covers the contract that every configured
// value reaches the settings.
func TestSettingsFromFullConfiguration(t *testing.T) {
	settings, enabled, err := SettingsFrom(config.UploadExperiment{
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
	if !enabled {
		t.Fatal("experiment is disabled although it is configured")
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

// TestSettingsFromDefaults covers the contract that the unset knobs fall back
// to the defaults of the experiment.
func TestSettingsFromDefaults(t *testing.T) {
	settings, enabled, err := SettingsFrom(config.UploadExperiment{Rate: config.ByteRate(1 << 20)})
	if err != nil {
		t.Fatalf("SettingsFrom: %v", err)
	}
	if !enabled {
		t.Fatal("experiment is disabled although a rate is configured")
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
}

// TestSettingsFromRejectsInvalidConfiguration covers the contract that a
// partially configured setup is rejected instead of falling back to the real
// counters.
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
		"jitter without experiment":   {Jitter: config.Percent(10)},
		"cap without experiment":      {Cap: config.OptionalByteQuantity(1 << 40)},
		"seed without experiment":     {Seed: 7},
		"interval without mode":       {PeerSampleInterval: second},
		"interval without log":        {Mode: "baseline", PeerSampleInterval: second},
		"jitter above one hundred":    {Rate: config.ByteRate(1 << 20), Jitter: config.Percent(101)},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := SettingsFrom(cfg); err == nil {
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
		"jitter without mode": {
			cfg:  config.UploadExperiment{Jitter: config.Percent(10)},
			want: []string{"experiments.upload.jitter"},
		},
		"interval without log": {
			cfg:  config.UploadExperiment{Mode: "baseline", PeerSampleInterval: config.OptionalDuration(time.Second)},
			want: []string{"experiments.upload.peer_sample_interval", "experiments.upload.announce_log"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := SettingsFrom(tc.cfg)
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
