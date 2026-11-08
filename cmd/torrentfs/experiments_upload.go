package main

import (
	"fmt"
	"log/slog"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/experiments"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// experimentSettings turns the [experiments.upload] configuration into the
// settings of the always-on experiment.
//
// The experiment has no enable switch: the configuration always yields
// settings, and an unconfigured experiment is the baseline arm. legacy, when
// non-nil, replaces the runtime settings of the configuration: it carries the
// values of the retired .metadata/lab_settings.json sidecar that could not be
// merged into the configuration file, so an upgrade does not change what the
// operator sees.
func experimentSettings(cfg config.Config, legacy *experiments.RuntimeSettings) (experiments.Settings, error) {
	settings, err := experiments.SettingsFrom(cfg.Experiments.Upload)
	if err != nil {
		return experiments.Settings{}, err
	}
	if legacy != nil {
		settings.RuntimeSettings = *legacy
	}
	return settings, nil
}

// labSessionOptions returns the session option that owns the upload statistics
// experiment. The experiment is always on, so the option is always returned;
// the session opens the optional announce log and starts the peer sampler only
// when the configuration names one.
//
// This is research tooling for exercising anomaly detection on a tracker you
// operate and must not be pointed at a third-party tracker.
func labSessionOptions(cfg config.Config, legacy *experiments.RuntimeSettings, logger *slog.Logger) ([]session.Option, error) {
	settings, err := experimentSettings(cfg, legacy)
	if err != nil {
		return nil, fmt.Errorf("experiments.upload experiment: %w", err)
	}
	logger.Info("upload statistics experiment running",
		"mode", settings.Mode,
		"rate_bytes_per_second", settings.RateBytesPerSecond,
		"multiplier", settings.Multiplier,
		"jitter_percent", settings.JitterFraction*100,
		"cap_bytes", settings.CapBytes,
		"announce_log", settings.AnnounceLogPath,
		"peer_sample_interval", settings.PeerSampleInterval.String(),
	)
	if settings.Mode != experiments.ModeBaseline {
		logger.Warn("the upload statistics experiment rewrites the reported upload; only use it against a tracker you operate",
			"mode", settings.Mode)
	}
	return []session.Option{session.WithLabExperiment(settings)}, nil
}
