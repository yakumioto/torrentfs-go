package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/yakumioto/torrentfs-go/internal/lab"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// labSessionOptions parses the TORRENTFS_LAB_* environment and, when the
// experiment is configured, returns the session option that owns it. The
// session restores persisted runtime settings, opens the optional announce log,
// and routes announces through the reporter; the Web UI can then change the
// runtime settings (mode, rate, multiplier, jitter, cap) without a restart.
//
// This is research tooling for exercising anomaly detection on a tracker you
// operate and must not be pointed at a third-party tracker.
func labSessionOptions(logger *slog.Logger) ([]session.Option, error) {
	settings, enabled, err := lab.LoadSettings(os.Getenv)
	if err != nil {
		return nil, fmt.Errorf("lab experiment: %w", err)
	}
	if !enabled {
		return nil, nil
	}
	logger.Warn("lab upload experiment configured; runtime settings can be changed from the Web UI",
		"mode", settings.Mode,
		"rate_bytes_per_second", settings.RateBytesPerSecond,
		"multiplier", settings.Multiplier,
		"jitter_percent", settings.JitterFraction*100,
		"cap_bytes", settings.CapBytes,
		"announce_log", settings.AnnounceLogPath,
		"peer_sample_interval", settings.PeerSampleInterval.String(),
	)
	return []session.Option{session.WithLabExperiment(settings)}, nil
}
