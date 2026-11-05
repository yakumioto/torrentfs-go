package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/yakumioto/torrentfs-go/internal/lab"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// labExperiment is the running state of an enabled upload experiment. A nil
// pointer means the experiment is off: no announce rewriting, no records.
//
// Two experiment modes shape the tracker-visible upload counter, plus a
// baseline control:
//
//   - baseline reports the client's real counters and only records ground truth;
//   - inject replaces the counter with time-based synthetic growth that is
//     deliberately unrelated to real traffic;
//   - amplify grows the counter by a multiplier of the real payload bytes that
//     were actually transferred since the previous announce.
type labExperiment struct {
	settings lab.Settings
	recorder *lab.Recorder
	reporter *lab.Reporter

	samplerCancel context.CancelFunc
	samplerDone   chan struct{}
}

// startLabExperiment reads the TORRENTFS_LAB_* environment and opens the
// optional announce log. It returns nil when nothing is configured.
//
// This is research tooling for exercising anomaly detection on a tracker you
// operate; it is documented in docs/development.md and must not be pointed at
// a third-party tracker.
func startLabExperiment(logger *slog.Logger) (*labExperiment, error) {
	settings, enabled, err := lab.LoadSettings(os.Getenv)
	if err != nil {
		return nil, fmt.Errorf("lab experiment: %w", err)
	}
	if !enabled {
		return nil, nil
	}
	var recorder *lab.Recorder
	if settings.AnnounceLogPath != "" {
		recorder, err = lab.OpenRecorder(settings.AnnounceLogPath, logger)
		if err != nil {
			return nil, fmt.Errorf("lab experiment: %w", err)
		}
		recorder.WriteSettings(settings)
	}
	logger.Warn("lab upload experiment enabled; only use this build against a tracker you operate",
		"mode", settings.Mode,
		"rate_bytes_per_second", settings.RateBytesPerSecond,
		"multiplier", settings.Multiplier,
		"jitter_percent", settings.JitterFraction*100,
		"cap_bytes", settings.CapBytes,
		"seed", settings.Seed,
		"announce_log", settings.AnnounceLogPath,
		"peer_sample_interval", settings.PeerSampleInterval.String(),
	)
	return &labExperiment{
		settings: settings,
		recorder: recorder,
		reporter: lab.NewReporter(settings, recorder),
	}, nil
}

// sessionOptions returns the announce request hook for the experiment.
func (e *labExperiment) sessionOptions() []session.Option {
	if e == nil {
		return nil
	}
	return []session.Option{session.WithAnnounceRequestDirector(e.reporter.Director)}
}

// startSampler begins recording peer gauges and real payload counters. It needs
// the announce log, which is the only destination for those records.
func (e *labExperiment) startSampler(parent context.Context, sess *session.Session) {
	if e == nil || e.recorder == nil {
		return
	}
	sampler := lab.NewPeerSampler(e.recorder, e.settings.PeerSampleInterval, func() []lab.PeerSnapshot {
		activities := sess.TorrentPeerActivities()
		snapshots := make([]lab.PeerSnapshot, 0, len(activities))
		for _, activity := range activities {
			snapshots = append(snapshots, lab.PeerSnapshot{
				InfoHash:         activity.InfoHash,
				UploadedBytes:    activity.UploadedBytes,
				DownloadedBytes:  activity.DownloadedBytes,
				TotalPeers:       activity.TotalPeers,
				PendingPeers:     activity.PendingPeers,
				ActivePeers:      activity.ActivePeers,
				ConnectedSeeders: activity.ConnectedSeeders,
			})
		}
		return snapshots
	})
	ctx, cancel := context.WithCancel(parent)
	e.samplerCancel = cancel
	e.samplerDone = make(chan struct{})
	go func() {
		defer close(e.samplerDone)
		sampler.Run(ctx)
	}()
}

// Close stops the sampler before closing the announce log so no record races
// the closed file.
func (e *labExperiment) Close() {
	if e == nil {
		return
	}
	if e.samplerCancel != nil {
		e.samplerCancel()
		<-e.samplerDone
	}
	if e.recorder != nil {
		_ = e.recorder.Close()
	}
}
