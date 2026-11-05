package lab

import (
	"testing"
	"time"
)

func getenvFunc(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func TestLoadSettingsDisabled(t *testing.T) {
	settings, enabled, err := LoadSettings(getenvFunc(nil))
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if enabled {
		t.Fatal("experiment is enabled without any environment variable")
	}
	if settings != (Settings{}) {
		t.Fatalf("settings = %+v, want the zero value", settings)
	}
}

func TestLoadSettingsInfersMode(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want Mode
	}{
		{name: "rate implies inject", env: map[string]string{EnvRate: "2MiB/s"}, want: ModeInject},
		{name: "multiplier implies amplify", env: map[string]string{EnvMultiplier: "5x"}, want: ModeAmplify},
		{name: "log implies baseline", env: map[string]string{EnvAnnounceLog: "announces.jsonl"}, want: ModeBaseline},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings, enabled, err := LoadSettings(getenvFunc(tc.env))
			if err != nil {
				t.Fatalf("LoadSettings: %v", err)
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

func TestLoadSettingsFullConfiguration(t *testing.T) {
	settings, enabled, err := LoadSettings(getenvFunc(map[string]string{
		EnvMode:               "inject",
		EnvRate:               "2MiB/s",
		EnvJitter:             "25%",
		EnvCap:                "3TiB",
		EnvSeed:               "42",
		EnvAnnounceLog:        "/tmp/announces.jsonl",
		EnvPeerSampleInterval: "2s",
	}))
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
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

func TestLoadSettingsDefaults(t *testing.T) {
	settings, enabled, err := LoadSettings(getenvFunc(map[string]string{EnvRate: "1MiB"}))
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
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

func TestLoadSettingsRejectsInvalidConfiguration(t *testing.T) {
	cases := map[string]map[string]string{
		"unknown mode":              {EnvMode: "turbo"},
		"inject without rate":       {EnvMode: "inject"},
		"amplify without factor":    {EnvMode: "amplify"},
		"baseline with rate":        {EnvMode: "baseline", EnvRate: "1MiB"},
		"baseline with multiplier":  {EnvMode: "baseline", EnvMultiplier: "2"},
		"inject with multiplier":    {EnvMode: "inject", EnvRate: "1MiB", EnvMultiplier: "2"},
		"amplify with rate":         {EnvMode: "amplify", EnvMultiplier: "2", EnvRate: "1MiB"},
		"jitter without experiment": {EnvJitter: "10"},
		"cap without experiment":    {EnvCap: "1TiB"},
		"seed without experiment":   {EnvSeed: "7"},
		"interval without mode":     {EnvPeerSampleInterval: "1s"},
		"interval without log":      {EnvMode: "baseline", EnvPeerSampleInterval: "1s"},
		"rate without unit":         {EnvRate: "2"},
		"rate zero":                 {EnvRate: "0MiB"},
		"multiplier at one":         {EnvMultiplier: "1"},
		"multiplier below one":      {EnvMultiplier: "0.5"},
		"multiplier not a number":   {EnvMultiplier: "many"},
		"jitter above 100":          {EnvRate: "1MiB", EnvJitter: "101"},
		"jitter not a number":       {EnvRate: "1MiB", EnvJitter: "lots"},
		"cap without unit":          {EnvRate: "1MiB", EnvCap: "1024"},
		"seed not an integer":       {EnvRate: "1MiB", EnvSeed: "later"},
		"interval zero":             {EnvMode: "baseline", EnvAnnounceLog: "x.jsonl", EnvPeerSampleInterval: "0s"},
		"interval not a duration":   {EnvMode: "baseline", EnvAnnounceLog: "x.jsonl", EnvPeerSampleInterval: "soon"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := LoadSettings(getenvFunc(env)); err == nil {
				t.Fatalf("LoadSettings(%v) = nil error, want rejection", env)
			}
		})
	}
}

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
