package lab

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

const (
	// EnvMode selects the experiment. An empty mode is inferred: a rate means
	// "inject", an announce log without a rate means "baseline", and a
	// multiplier alone means "amplify".
	EnvMode = "TORRENTFS_LAB_UPLOAD_MODE"
	// EnvRate enables inject mode and sets its average synthetic rate. The
	// value is a byte quantity, optionally suffixed with "/s": "2MiB/s" and
	// "2MiB" both mean two mebibytes per second.
	EnvRate = "TORRENTFS_LAB_UPLOAD_RATE"
	// EnvMultiplier enables amplify mode: the reported upload grows by this
	// factor of the real payload bytes transferred since the previous announce.
	EnvMultiplier = "TORRENTFS_LAB_UPLOAD_MULTIPLIER"
	// EnvJitter is the per-announce deviation from the nominal increment, in
	// percent (0-100). The default is 20 percent.
	EnvJitter = "TORRENTFS_LAB_UPLOAD_JITTER"
	// EnvCap is the per-torrent ceiling on the cumulative reported upload.
	EnvCap = "TORRENTFS_LAB_UPLOAD_CAP"
	// EnvSeed fixes the random jitter sequence for reproducible runs.
	EnvSeed = "TORRENTFS_LAB_UPLOAD_SEED"
	// EnvAnnounceLog is a JSON Lines file that receives one record per tracker
	// announce plus periodic peer-activity samples. It is the ground truth used
	// to compare reported statistics with real payload transfer.
	EnvAnnounceLog = "TORRENTFS_LAB_ANNOUNCE_LOG"
	// EnvPeerSampleInterval is how often peer gauges and real payload counters
	// are sampled into the announce log. The default is 5s.
	EnvPeerSampleInterval = "TORRENTFS_LAB_PEER_SAMPLE_INTERVAL"
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

// Settings is the validated experiment configuration.
type Settings struct {
	Mode Mode
	// RateBytesPerSecond is the average growth of the reported cumulative
	// upload in inject mode.
	RateBytesPerSecond float64
	// Multiplier is the reported-to-real factor in amplify mode.
	Multiplier float64
	// JitterFraction is the maximum deviation from the nominal increment for a
	// single announce, in the range [0, 1].
	JitterFraction float64
	// CapBytes is the per-torrent ceiling on the reported cumulative upload.
	// Zero means uncapped.
	CapBytes int64
	// Seed seeds the jitter sequence.
	Seed int64
	// AnnounceLogPath is the JSON Lines ground-truth log. Empty disables it.
	AnnounceLogPath string
	// PeerSampleInterval is the sampling period for peer activity.
	PeerSampleInterval time.Duration
}

// Enabled reports whether an experiment mode was configured.
func (s Settings) Enabled() bool { return s.Mode != "" }

// LoadSettings reads the lab environment variables through getenv. enabled is
// false with a nil error when nothing is configured. A partially configured
// setup is an error so a typo cannot silently fall back to normal announce
// reporting.
func LoadSettings(getenv func(string) string) (settings Settings, enabled bool, err error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	rawMode := strings.ToLower(strings.TrimSpace(getenv(EnvMode)))
	rawRate := strings.TrimSpace(getenv(EnvRate))
	rawMultiplier := strings.TrimSpace(getenv(EnvMultiplier))
	rawLog := strings.TrimSpace(getenv(EnvAnnounceLog))
	rawInterval := strings.TrimSpace(getenv(EnvPeerSampleInterval))
	rawJitter := strings.TrimSpace(getenv(EnvJitter))
	rawCap := strings.TrimSpace(getenv(EnvCap))
	rawSeed := strings.TrimSpace(getenv(EnvSeed))

	mode := Mode(rawMode)
	switch mode {
	case "":
		switch {
		case rawRate != "":
			mode = ModeInject
		case rawMultiplier != "":
			mode = ModeAmplify
		case rawLog != "":
			mode = ModeBaseline
		default:
			for _, knob := range []struct {
				key   string
				value string
			}{
				{EnvJitter, rawJitter},
				{EnvCap, rawCap},
				{EnvSeed, rawSeed},
				{EnvPeerSampleInterval, rawInterval},
			} {
				if knob.value != "" {
					return Settings{}, false, fmt.Errorf("%s is set but no experiment mode, rate, or announce log is configured", knob.key)
				}
			}
			return Settings{}, false, nil
		}
	case ModeBaseline, ModeInject, ModeAmplify:
	default:
		return Settings{}, false, fmt.Errorf("%s: unknown mode %q (want baseline, inject, or amplify)", EnvMode, rawMode)
	}

	settings = Settings{
		Mode:               mode,
		JitterFraction:     float64(defaultJitterPercent) / 100,
		Seed:               time.Now().UnixNano(),
		AnnounceLogPath:    rawLog,
		PeerSampleInterval: defaultPeerSampleInterval,
	}

	switch mode {
	case ModeBaseline:
		if rawRate != "" || rawMultiplier != "" {
			return Settings{}, false, fmt.Errorf("mode %q reports the real counters and does not use %s or %s", mode, EnvRate, EnvMultiplier)
		}
	case ModeInject:
		if rawRate == "" {
			return Settings{}, false, fmt.Errorf("mode %q requires %s", mode, EnvRate)
		}
		if rawMultiplier != "" {
			return Settings{}, false, fmt.Errorf("%s only applies to mode %q", EnvMultiplier, ModeAmplify)
		}
		rate, err := parseByteRate(rawRate)
		if err != nil {
			return Settings{}, false, err
		}
		settings.RateBytesPerSecond = rate
	case ModeAmplify:
		if rawMultiplier == "" {
			return Settings{}, false, fmt.Errorf("mode %q requires %s", mode, EnvMultiplier)
		}
		if rawRate != "" {
			return Settings{}, false, fmt.Errorf("%s only applies to mode %q", EnvRate, ModeInject)
		}
		multiplier, err := parseMultiplier(rawMultiplier)
		if err != nil {
			return Settings{}, false, err
		}
		settings.Multiplier = multiplier
	}

	if rawJitter != "" {
		jitter, err := parseJitterPercent(rawJitter)
		if err != nil {
			return Settings{}, false, err
		}
		settings.JitterFraction = jitter
	}
	if rawCap != "" {
		capBytes, err := config.ParseByteQuantity(rawCap)
		if err != nil {
			return Settings{}, false, fmt.Errorf("%s: %w", EnvCap, err)
		}
		settings.CapBytes = capBytes
	}
	if rawSeed != "" {
		seed, err := strconv.ParseInt(rawSeed, 10, 64)
		if err != nil {
			return Settings{}, false, fmt.Errorf("%s: must be a signed integer", EnvSeed)
		}
		settings.Seed = seed
	}
	if rawInterval != "" {
		if rawLog == "" {
			return Settings{}, false, fmt.Errorf("%s requires %s", EnvPeerSampleInterval, EnvAnnounceLog)
		}
		interval, err := time.ParseDuration(rawInterval)
		if err != nil || interval <= 0 {
			return Settings{}, false, fmt.Errorf("%s: must be a positive duration such as 5s", EnvPeerSampleInterval)
		}
		settings.PeerSampleInterval = interval
	}
	return settings, true, nil
}

func parseByteRate(raw string) (float64, error) {
	value := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "/s"))
	bytes, err := config.ParseByteQuantity(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", EnvRate, err)
	}
	if bytes <= 0 {
		return 0, fmt.Errorf("%s: must be greater than zero", EnvRate)
	}
	return float64(bytes), nil
}

func parseMultiplier(raw string) (float64, error) {
	value := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "x"))
	multiplier, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(multiplier) || math.IsInf(multiplier, 0) {
		return 0, fmt.Errorf("%s: must be a number greater than 1", EnvMultiplier)
	}
	if multiplier <= 1 {
		return 0, fmt.Errorf("%s: must be greater than 1", EnvMultiplier)
	}
	return multiplier, nil
}

func parseJitterPercent(raw string) (float64, error) {
	value := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "%"))
	percent, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(percent) || math.IsInf(percent, 0) {
		return 0, fmt.Errorf("%s: must be a number between 0 and 100", EnvJitter)
	}
	if percent < 0 || percent > 100 {
		return 0, fmt.Errorf("%s: must be between 0 and 100 percent", EnvJitter)
	}
	return percent / 100, nil
}
