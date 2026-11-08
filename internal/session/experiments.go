package session

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/experiments"
)

// The experiment is always on: it has no enable/disable state and persists no
// settings of its own. The [experiments.upload] configuration owns the values,
// the configuration page hot-applies the runtime ones, and the startup
// migration imports a retired .metadata/lab_settings.json sidecar.

var (
	// ErrInvalidLabSettings reports a value that violates the experiment
	// contract. The wrapped cause names the offending field.
	ErrInvalidLabSettings = errors.New("session: invalid lab settings")
)

// LabSettings is the runtime-tunable part of the experiment, exposed by the
// management API and applied by the configuration page.
type LabSettings = experiments.RuntimeSettings

// labState is the session-owned experiment. It is always present.
type labState struct {
	// settings holds the values that are fixed for this process.
	settings experiments.Settings
	reporter *experiments.Reporter
	recorder *experiments.Recorder
}

// WithLabExperiment sets the experiment settings the session starts with. The
// experiment itself is always on; without the option the session starts in the
// baseline arm.
func WithLabExperiment(settings experiments.Settings) Option {
	return func(options *sessionOptions) {
		settings := settings
		options.labSettings = &settings
	}
}

// startLabSampler records peer activity and real payload counters into the
// announce log. It joins the session's background lifecycle, so Close cancels
// it and waits before the recorder is closed. A session whose configuration
// names no announce log has no recorder and starts no sampler.
func (s *Session) startLabSampler(state *labState) {
	if state == nil || state.recorder == nil {
		return
	}
	sampler := experiments.NewPeerSampler(state.recorder, state.settings.PeerSampleInterval, func() []experiments.PeerSnapshot {
		activities := s.TorrentPeerActivities()
		snapshots := make([]experiments.PeerSnapshot, 0, len(activities))
		for _, activity := range activities {
			snapshots = append(snapshots, experiments.PeerSnapshot{
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
	s.bgMu.Lock()
	s.bgWg.Add(1)
	s.bgMu.Unlock()
	go func() {
		defer s.bgWg.Done()
		sampler.Run(s.bgCtx)
	}()
}

// LabTorrentStatus is one torrent's live real counter, the value the same
// counter had when the last announce was sent, and the tracker-reported value.
// Only torrents that have announced at least once in this process appear.
type LabTorrentStatus struct {
	InfoHash          string
	RealUploadedBytes int64
	// RealUploadedAtLastAnnounceBytes is the real payload counter at the time the
	// last announce was sent. Over-reporting must be measured against this value,
	// not the live counter: bytes uploaded after the announce are not visible to
	// the tracker until the next one.
	RealUploadedAtLastAnnounceBytes int64
	ReportedUploadedBytes           int64
	LastAnnounceAt                  time.Time
}

// LabStatus is the management API's view of the experiment.
type LabStatus struct {
	Settings           experiments.RuntimeSettings
	Seed               int64
	AnnounceLogPath    string
	PeerSampleInterval time.Duration
	Torrents           []LabTorrentStatus
}

// LabStatus returns the experiment configuration and the per-torrent real and
// reported upload counters. The experiment is always on, so a session always
// reports its settings.
func (s *Session) LabStatus() LabStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state := s.lab
	if state == nil {
		return LabStatus{Torrents: []LabTorrentStatus{}}
	}
	states := state.reporter.ReportedStates()
	torrents := make([]LabTorrentStatus, 0, len(s.torrents))
	for hash, handle := range s.torrents {
		reported, ok := states[hash.HexString()]
		if !ok {
			continue
		}
		var realBytes int64
		if handle != nil {
			stats := handle.tor.Stats()
			realBytes = stats.ConnStats.BytesWrittenData.Int64()
		}
		torrents = append(torrents, LabTorrentStatus{
			InfoHash:                        hash.HexString(),
			RealUploadedBytes:               realBytes,
			RealUploadedAtLastAnnounceBytes: reported.RealUploadedAtAnnounce,
			ReportedUploadedBytes:           reported.ReportedBytes,
			LastAnnounceAt:                  reported.LastAnnounceAt,
		})
	}
	sort.Slice(torrents, func(i, j int) bool { return torrents[i].InfoHash < torrents[j].InfoHash })
	return LabStatus{
		Settings:           state.reporter.RuntimeSettings(),
		Seed:               state.settings.Seed,
		AnnounceLogPath:    state.settings.AnnounceLogPath,
		PeerSampleInterval: state.settings.PeerSampleInterval,
		Torrents:           torrents,
	}
}

// SetLabSettings validates and applies runtime experiment settings and returns
// the normalized value. It changes only the running reporter: the configuration
// file is the sole persistence, written by the configuration save path.
//
// The caller must already have validated the mode/rate combination through
// NormalizeRuntimeSettings; this method validates again so a caller cannot
// bypass it.
func (s *Session) SetLabSettings(ctx context.Context, settings experiments.RuntimeSettings) (experiments.RuntimeSettings, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return experiments.RuntimeSettings{}, err
	}
	normalized, err := experiments.NormalizeRuntimeSettings(settings)
	if err != nil {
		return experiments.RuntimeSettings{}, fmt.Errorf("%w: %v", ErrInvalidLabSettings, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureActiveLocked(); err != nil {
		return experiments.RuntimeSettings{}, err
	}
	state := s.lab
	if state == nil {
		return experiments.RuntimeSettings{}, ErrInvalidLabSettings
	}
	if err := ctx.Err(); err != nil {
		return experiments.RuntimeSettings{}, err
	}
	state.reporter.UpdateRuntime(normalized)
	return normalized, nil
}

// experimentHotApplyKeys are the [experiments.upload] keys the running
// reporter can replace without a restart. The seed, the announce log, and the
// peer sample interval stay fixed for the lifetime of the process, so they are
// restart-required.
var experimentHotApplyKeys = []string{
	"experiments.upload.mode",
	"experiments.upload.rate",
	"experiments.upload.multiplier",
	"experiments.upload.jitter",
	"experiments.upload.cap",
}

// ExperimentApplier applies the hot-applied [experiments.upload] keys to the
// session's always-on reporter.
type ExperimentApplier struct {
	session *Session
}

// NewExperimentApplier builds the configuration applier of an experiment
// session.
func NewExperimentApplier(s *Session) *ExperimentApplier {
	return &ExperimentApplier{session: s}
}

// Keys returns the configuration keys the applier can replace.
func (a *ExperimentApplier) Keys() []string {
	return append([]string{}, experimentHotApplyKeys...)
}

// Apply applies the runtime part of the experiment configuration to the
// reporter.
func (a *ExperimentApplier) Apply(cfg config.Config) error {
	settings, err := experiments.SettingsFrom(cfg.Experiments.Upload)
	if err != nil {
		return err
	}
	_, err = a.session.SetLabSettings(context.Background(), settings.RuntimeSettings)
	return err
}
