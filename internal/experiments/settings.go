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

// DefaultSettings returns the settings of the always-on experiment when the
// configuration names nothing: baseline mode, the default jitter, a wall-clock
// seed, and no announce log.
func DefaultSettings() Settings {
	settings, err := SettingsFrom(config.Default().Experiments.Upload)
	if err != nil {
		// The built-in defaults are valid by construction; keep the fallback
		// explicit so a future default cannot make the session unusable.
		return Settings{RuntimeSettings: RuntimeSettings{Mode: ModeBaseline, JitterFraction: float64(defaultJitterPercent) / 100}}
	}
	return settings
}

// SettingsFrom turns the configuration of the experiment into its settings.
//
// The configuration holds the values and checks their shape; the rules that
// relate one setting to another live here, because they decide what a set of
// settings means. The experiment is always on, so an unconfigured experiment
// is the baseline arm. A mode is never inferred: a rate or multiplier without
// its mode is an error that names the mode to add, so a mistyped or obsolete
// configuration cannot silently change what the tracker sees.
func SettingsFrom(cfg config.UploadExperiment) (Settings, error) {
	mode := Mode(strings.ToLower(strings.TrimSpace(cfg.Mode)))
	if mode == "" {
		mode = ModeBaseline
	}
	switch mode {
	case ModeBaseline, ModeInject, ModeAmplify:
	default:
		return Settings{}, fmt.Errorf("experiments.upload.mode: unknown mode %q (want baseline, inject, or amplify)", cfg.Mode)
	}

	runtime := RuntimeSettings{Mode: mode}
	switch mode {
	case ModeBaseline:
		if cfg.Rate != 0 {
			return Settings{}, fmt.Errorf("experiments.upload.rate is set but experiments.upload.mode is %q; set experiments.upload.mode = %q to use the rate", ModeBaseline, ModeInject)
		}
		if cfg.Multiplier != 0 {
			return Settings{}, fmt.Errorf("experiments.upload.multiplier is set but experiments.upload.mode is %q; set experiments.upload.mode = %q to use the multiplier", ModeBaseline, ModeAmplify)
		}
	case ModeInject:
		if cfg.Rate == 0 {
			return Settings{}, fmt.Errorf("experiments.upload.rate must be positive in mode %q", ModeInject)
		}
		if cfg.Multiplier != 0 {
			return Settings{}, fmt.Errorf("experiments.upload.multiplier only applies to mode %q; experiments.upload.mode is %q", ModeAmplify, ModeInject)
		}
		runtime.RateBytesPerSecond = float64(cfg.Rate)
	case ModeAmplify:
		if cfg.Multiplier <= 1 {
			return Settings{}, fmt.Errorf("experiments.upload.multiplier must be greater than 1 in mode %q", ModeAmplify)
		}
		if cfg.Rate != 0 {
			return Settings{}, fmt.Errorf("experiments.upload.rate only applies to mode %q; experiments.upload.mode is %q", ModeInject, ModeAmplify)
		}
		runtime.Multiplier = float64(cfg.Multiplier)
	}
	runtime.JitterFraction = float64(cfg.Jitter) / 100
	runtime.CapBytes = int64(cfg.Cap)
	runtime, err := NormalizeRuntimeSettings(runtime)
	if err != nil {
		return Settings{}, err
	}

	seed := time.Now().UnixNano()
	if cfg.Seed != 0 {
		seed = cfg.Seed
	}
	peerSampleInterval := defaultPeerSampleInterval
	if cfg.PeerSampleInterval != 0 {
		if cfg.AnnounceLog == "" {
			return Settings{}, fmt.Errorf("experiments.upload.peer_sample_interval requires experiments.upload.announce_log")
		}
		peerSampleInterval = time.Duration(cfg.PeerSampleInterval)
	}
	return Settings{
		RuntimeSettings:    runtime,
		Seed:               seed,
		AnnounceLogPath:    cfg.AnnounceLog,
		PeerSampleInterval: peerSampleInterval,
	}, nil
}
