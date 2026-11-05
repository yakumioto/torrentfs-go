package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/yakumioto/torrentfs-go/internal/lab"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// syntheticUploadOptions enables the experimental synthetic announce uploader
// when its TORRENTFS_LAB_FAKE_UPLOAD_* environment variables are set. It
// returns no options otherwise, so a deployment that does not opt in keeps
// reporting the client's real payload counters.
//
// This is research tooling for exercising anomaly detection on a tracker you
// operate; it is documented in docs/development.md and must not be pointed at
// a third-party tracker.
func syntheticUploadOptions(logger *slog.Logger) ([]session.Option, error) {
	settings, enabled, err := lab.LoadUploadSettings(os.Getenv)
	if err != nil {
		return nil, fmt.Errorf("synthetic announce upload: %w", err)
	}
	if !enabled {
		return nil, nil
	}
	reporter := lab.NewUploadReporter(settings)
	logger.Warn("synthetic announce upload reporting is enabled; only use this build against a tracker you operate",
		"rate_bytes_per_second", settings.RateBytesPerSecond,
		"jitter_percent", settings.JitterFraction*100,
		"cap_bytes", settings.CapBytes,
		"seed", settings.Seed,
	)
	return []session.Option{session.WithAnnounceRequestDirector(reporter.Director)}, nil
}
