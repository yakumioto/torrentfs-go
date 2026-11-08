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
// settings, and an unconfigured experiment is the baseline arm. A read-only
// start additionally overlays the retired .metadata/lab_settings.json sidecar
// onto the configuration before this call, so the session and the
// configuration page use the same values.
func experimentSettings(cfg config.Config) (experiments.Settings, error) {
	return experiments.SettingsFrom(cfg.Experiments.Upload)
}

// legacyExperimentUpload returns the [experiments.upload] values of the
// retired sidecar.
//
// The file migration and the read-only runtime fallback both map the same
// sidecar record, so the conversion lives in one place; only the rule for
// which keys may be replaced differs between the two paths.
func legacyExperimentUpload(settings experiments.RuntimeSettings) config.UploadExperiment {
	return config.UploadExperiment{
		Mode:       string(settings.Mode),
		Rate:       config.ByteRate(settings.RateBytesPerSecond),
		Multiplier: config.Ratio(settings.Multiplier),
		Jitter:     config.Percent(settings.JitterFraction * 100),
		Cap:        config.OptionalByteQuantity(settings.CapBytes),
	}
}

// applyLegacyExperimentRuntime overlays the retired sidecar values onto the
// configuration a read-only start uses.
//
// The environment is a configuration source and wins over the retired sidecar,
// so a key it supplies keeps its value and source. The applied keys are
// recorded as file values: the source model has only default, file, and
// environment, and the sidecar is a persisted file the operator supplied. The
// page is read-only in every deployment that reaches this path, and the
// upgraded configuration file replaces that source on the next writable start.
func applyLegacyExperimentRuntime(cfg *config.Config, sources config.Sources, legacy *experiments.RuntimeSettings) config.Sources {
	if legacy == nil {
		return sources
	}
	if sources == nil {
		sources = config.Sources{}
	}
	legacyUpload := legacyExperimentUpload(*legacy)
	upload := cfg.Experiments.Upload
	if sources.SourceOf("experiments.upload.mode") != config.SourceEnvironment {
		upload.Mode = legacyUpload.Mode
		sources["experiments.upload.mode"] = config.SourceFile
	}
	if sources.SourceOf("experiments.upload.rate") != config.SourceEnvironment {
		upload.Rate = legacyUpload.Rate
		sources["experiments.upload.rate"] = config.SourceFile
	}
	if sources.SourceOf("experiments.upload.multiplier") != config.SourceEnvironment {
		upload.Multiplier = legacyUpload.Multiplier
		sources["experiments.upload.multiplier"] = config.SourceFile
	}
	if sources.SourceOf("experiments.upload.jitter") != config.SourceEnvironment {
		upload.Jitter = legacyUpload.Jitter
		sources["experiments.upload.jitter"] = config.SourceFile
	}
	if sources.SourceOf("experiments.upload.cap") != config.SourceEnvironment {
		upload.Cap = legacyUpload.Cap
		sources["experiments.upload.cap"] = config.SourceFile
	}
	cfg.Experiments.Upload = upload
	return sources
}

// experimentSessionOptions returns the session option that owns the upload statistics
// experiment. The experiment is always on, so the option is always returned;
// the session opens the optional announce log and starts the peer sampler only
// when the configuration names one.
//
// This is research tooling for exercising anomaly detection on a tracker you
// operate and must not be pointed at a third-party tracker.
func experimentSessionOptions(cfg config.Config, logger *slog.Logger) ([]session.Option, error) {
	settings, err := experimentSettings(cfg)
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
	return []session.Option{session.WithExperiment(settings)}, nil
}
