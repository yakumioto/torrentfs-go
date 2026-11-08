package experiments

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// Mode selects how the upload counter reported to trackers relates to the real
// payload transfer.
type Mode string

const (
	// ModeBaseline reports the client's real counters. It is the control arm:
	// nothing is rewritten, but announce-log and peer-sampling records still
	// capture ground truth.
	ModeBaseline Mode = "baseline"
	// ModeInject replaces the reported upload with a purely time-based
	// synthetic counter. Growth is independent of real peer traffic; this is
	// the deliberate anomaly used to test whether a tracker can flag it.
	ModeInject Mode = "inject"
	// ModeAmplify grows the reported upload by a multiplier of the real payload
	// bytes sent since the previous announce. Growth only happens when the
	// torrent actually transferred data, which produces the coupled adversary
	// model: an inflated but causally plausible counter.
	ModeAmplify Mode = "amplify"
)

const (
	defaultJitterPercent      = 20
	defaultPeerSampleInterval = 5 * time.Second
)

// RuntimeSettings is the part of the experiment that can change while the
// service runs, through the management API.
type RuntimeSettings struct {
	// Mode selects the reporting model. One of the Mode constants.
	Mode Mode `json:"mode"`
	// RateBytesPerSecond is the average growth of the reported cumulative
	// upload in inject mode.
	RateBytesPerSecond float64 `json:"rate_bytes_per_second"`
	// Multiplier is the reported-to-real factor in amplify mode.
	Multiplier float64 `json:"multiplier"`
	// JitterFraction is the maximum deviation from the nominal increment for a
	// single announce, in the range [0, 1].
	JitterFraction float64 `json:"jitter_fraction"`
	// CapBytes is the per-torrent ceiling on the reported cumulative upload.
	// Zero means uncapped.
	CapBytes int64 `json:"cap_bytes"`
}

// NormalizeRuntimeSettings validates runtime settings and clears fields that do
// not apply to the selected mode, so a mode switch never carries a stale value
// into a mode that would otherwise use it.
func NormalizeRuntimeSettings(settings RuntimeSettings) (RuntimeSettings, error) {
	switch settings.Mode {
	case ModeBaseline, ModeInject, ModeAmplify:
	default:
		return RuntimeSettings{}, errors.New("mode must be baseline, inject, or amplify")
	}
	if math.IsNaN(settings.JitterFraction) || math.IsInf(settings.JitterFraction, 0) ||
		settings.JitterFraction < 0 || settings.JitterFraction > 1 {
		return RuntimeSettings{}, errors.New("jitter_percent must be between 0 and 100")
	}
	if settings.CapBytes < 0 {
		return RuntimeSettings{}, errors.New("cap_bytes must not be negative")
	}
	normalized := RuntimeSettings{
		Mode:           settings.Mode,
		JitterFraction: settings.JitterFraction,
		CapBytes:       settings.CapBytes,
	}
	switch settings.Mode {
	case ModeInject:
		if math.IsNaN(settings.RateBytesPerSecond) || math.IsInf(settings.RateBytesPerSecond, 0) ||
			settings.RateBytesPerSecond <= 0 {
			return RuntimeSettings{}, errors.New("inject mode requires rate_bytes_per_second greater than zero")
		}
		normalized.RateBytesPerSecond = settings.RateBytesPerSecond
	case ModeAmplify:
		if math.IsNaN(settings.Multiplier) || math.IsInf(settings.Multiplier, 0) || settings.Multiplier <= 1 {
			return RuntimeSettings{}, errors.New("amplify mode requires multiplier greater than 1")
		}
		normalized.Multiplier = settings.Multiplier
	}
	return normalized, nil
}

// Settings is the startup configuration of the experiment: runtime settings
// plus the values that are fixed for the lifetime of the process.
type Settings struct {
	RuntimeSettings
	// Seed seeds the jitter sequence. It is fixed for the process because
	// changing it would make an in-flight run impossible to reproduce.
	Seed int64
	// AnnounceLogPath is the JSON Lines ground-truth log. Empty disables it.
	AnnounceLogPath string
	// PeerSampleInterval is the sampling period for peer activity.
	PeerSampleInterval time.Duration
}

// Enabled reports whether an experiment mode was configured.
func (s Settings) Enabled() bool { return s.Mode != "" }

// SettingsFrom turns the configuration of the experiment into its settings.
//
// The configuration holds the values and checks their shape; the rules that
// relate one setting to another live here, because they decide what a set of
// settings means. enabled is false with a nil error when nothing is
// configured. A partially configured setup is an error so a typo cannot
// silently fall back to normal announce reporting.
func SettingsFrom(cfg config.Lab) (settings Settings, enabled bool, err error) {
	mode := Mode(strings.ToLower(strings.TrimSpace(cfg.UploadMode)))
	switch mode {
	case "":
		switch {
		case cfg.UploadRate != 0:
			mode = ModeInject
		case cfg.UploadMultiplier != 0:
			mode = ModeAmplify
		case cfg.AnnounceLog != "":
			mode = ModeBaseline
		default:
			for _, knob := range []struct {
				key   string
				value bool
			}{
				{"upload_jitter", cfg.UploadJitter != 0},
				{"upload_cap", cfg.UploadCap != 0},
				{"upload_seed", cfg.UploadSeed != 0},
				{"peer_sample_interval", cfg.PeerSampleInterval != 0},
			} {
				if knob.value {
					return Settings{}, false, fmt.Errorf("lab.%s is set but no experiment mode, rate, or announce log is configured", knob.key)
				}
			}
			return Settings{}, false, nil
		}
	case ModeBaseline, ModeInject, ModeAmplify:
	default:
		return Settings{}, false, fmt.Errorf("lab.upload_mode: unknown mode %q (want baseline, inject, or amplify)", cfg.UploadMode)
	}

	runtime := RuntimeSettings{
		Mode:           mode,
		JitterFraction: float64(defaultJitterPercent) / 100,
	}
	switch mode {
	case ModeBaseline:
		if cfg.UploadRate != 0 || cfg.UploadMultiplier != 0 {
			return Settings{}, false, fmt.Errorf("mode %q reports the real counters and does not use upload_rate or upload_multiplier", mode)
		}
	case ModeInject:
		if cfg.UploadRate == 0 {
			return Settings{}, false, fmt.Errorf("mode %q requires upload_rate", mode)
		}
		if cfg.UploadMultiplier != 0 {
			return Settings{}, false, fmt.Errorf("upload_multiplier only applies to mode %q", ModeAmplify)
		}
		runtime.RateBytesPerSecond = float64(cfg.UploadRate)
	case ModeAmplify:
		if cfg.UploadMultiplier == 0 {
			return Settings{}, false, fmt.Errorf("mode %q requires upload_multiplier", mode)
		}
		if cfg.UploadRate != 0 {
			return Settings{}, false, fmt.Errorf("upload_rate only applies to mode %q", ModeInject)
		}
		runtime.Multiplier = float64(cfg.UploadMultiplier)
	}
	if cfg.UploadJitter != 0 {
		runtime.JitterFraction = float64(cfg.UploadJitter) / 100
	}
	runtime.CapBytes = int64(cfg.UploadCap)
	runtime, err = NormalizeRuntimeSettings(runtime)
	if err != nil {
		return Settings{}, false, err
	}

	seed := time.Now().UnixNano()
	if cfg.UploadSeed != 0 {
		seed = cfg.UploadSeed
	}
	peerSampleInterval := defaultPeerSampleInterval
	if cfg.PeerSampleInterval != 0 {
		if cfg.AnnounceLog == "" {
			return Settings{}, false, fmt.Errorf("lab.peer_sample_interval requires lab.announce_log")
		}
		peerSampleInterval = time.Duration(cfg.PeerSampleInterval)
	}
	return Settings{
		RuntimeSettings:    runtime,
		Seed:               seed,
		AnnounceLogPath:    cfg.AnnounceLog,
		PeerSampleInterval: peerSampleInterval,
	}, true, nil
}
