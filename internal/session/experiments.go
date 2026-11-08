package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/experiments"
)

// Lab experiment settings are per-torrents-directory state, not static
// configuration: the Web UI writes them through the management API and a
// restart restores them before the BitTorrent client is created.
var (
	// ErrInvalidLabSettings reports a value that violates the experiment
	// contract. The wrapped cause names the offending field.
	ErrInvalidLabSettings = errors.New("session: invalid lab settings")
	// ErrLabSettingsStorageUnavailable reports that the settings were not
	// published: the previous file and the running experiment are unchanged.
	ErrLabSettingsStorageUnavailable = errors.New("session: lab settings storage unavailable")
	// ErrLabSettingsDurabilityUnconfirmed reports that the new settings are
	// published and applied, but the directory could not be synced, so a
	// restart is not guaranteed to observe them.
	ErrLabSettingsDurabilityUnconfirmed = errors.New("session: lab settings durability unconfirmed")
	// ErrLabDisabled reports an update attempt when the session was not created
	// with WithLabExperiment.
	ErrLabDisabled = errors.New("session: lab experiment is not enabled")
)

const (
	labSettingsFileName = "lab_settings.json"
	labSettingsVersion  = 1
)

// LabSettings is the runtime-tunable part of the experiment, exposed by the
// management API.
type LabSettings = experiments.RuntimeSettings

// labState is nil unless the session was created with WithLabExperiment.
type labState struct {
	// settings holds the values that are fixed for this process.
	settings experiments.Settings
	reporter *experiments.Reporter
	recorder *experiments.Recorder
}

// labSettingsRecord is the on-disk schema. The version stays separate from the
// public wire contract.
type labSettingsRecord struct {
	Version            int              `json:"version"`
	Mode               experiments.Mode `json:"mode"`
	RateBytesPerSecond float64          `json:"rate_bytes_per_second"`
	Multiplier         float64          `json:"multiplier"`
	JitterFraction     float64          `json:"jitter_fraction"`
	CapBytes           int64            `json:"cap_bytes"`
}

func labSettingsPath(metadataDir string) string {
	return filepath.Join(metadataDir, labSettingsFileName)
}

// WithLabExperiment enables the experimental upload-statistics harness. The
// settings are validated by the caller (experiments.SettingsFrom). The session opens
// the optional announce log, restores any persisted runtime settings, and
// routes tracker announces through the reporter; the Web UI can then change the
// runtime settings through SetLabSettings.
func WithLabExperiment(settings experiments.Settings) Option {
	return func(options *sessionOptions) {
		settings := settings
		options.labSettings = &settings
	}
}

// loadLabSettings restores the persisted runtime settings, falling back to the
// startup values when no sidecar exists. persisted reports which source won.
func loadLabSettings(metadataDir string, fallback experiments.RuntimeSettings) (settings experiments.RuntimeSettings, persisted bool, err error) {
	path := labSettingsPath(metadataDir)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		normalized, normErr := experiments.NormalizeRuntimeSettings(fallback)
		if normErr != nil {
			return experiments.RuntimeSettings{}, false, fmt.Errorf("%w: %v", ErrInvalidLabSettings, normErr)
		}
		return normalized, false, nil
	} else if err != nil {
		return experiments.RuntimeSettings{}, false, fmt.Errorf("session: inspect %s: %w", path, err)
	}
	if _, err := requireRegularFile(path); err != nil {
		return experiments.RuntimeSettings{}, false, fmt.Errorf("session: %s: %w", labSettingsFileName, err)
	}
	file, err := os.Open(path)
	if err != nil {
		return experiments.RuntimeSettings{}, false, fmt.Errorf("session: read %s: %w", labSettingsFileName, err)
	}
	defer func() { _ = file.Close() }()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var record labSettingsRecord
	if err := decoder.Decode(&record); err != nil {
		return experiments.RuntimeSettings{}, false, fmt.Errorf("session: decode %s: %w", labSettingsFileName, err)
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return experiments.RuntimeSettings{}, false, fmt.Errorf("session: decode %s: unexpected trailing data", labSettingsFileName)
	}
	if record.Version != labSettingsVersion {
		return experiments.RuntimeSettings{}, false, fmt.Errorf("session: %s: unsupported version %d", labSettingsFileName, record.Version)
	}
	normalized, err := experiments.NormalizeRuntimeSettings(experiments.RuntimeSettings{
		Mode:               record.Mode,
		RateBytesPerSecond: record.RateBytesPerSecond,
		Multiplier:         record.Multiplier,
		JitterFraction:     record.JitterFraction,
		CapBytes:           record.CapBytes,
	})
	if err != nil {
		return experiments.RuntimeSettings{}, false, fmt.Errorf("%w: %v", ErrInvalidLabSettings, err)
	}
	return normalized, true, nil
}

// saveLabSettings atomically publishes runtime settings. The returned published
// flag reports whether the rename committed; when it did, a restart observes
// the new value even if the directory sync failed.
func saveLabSettings(metadataDir string, settings experiments.RuntimeSettings) (published bool, err error) {
	path := labSettingsPath(metadataDir)
	if _, err := os.Lstat(path); err == nil {
		if _, err := requireRegularFile(path); err != nil {
			return false, fmt.Errorf("%w: %v", ErrLabSettingsStorageUnavailable, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("%w: %v", ErrLabSettingsStorageUnavailable, err)
	}
	normalized, err := experiments.NormalizeRuntimeSettings(settings)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrInvalidLabSettings, err)
	}
	data, err := json.Marshal(labSettingsRecord{
		Version:            labSettingsVersion,
		Mode:               normalized.Mode,
		RateBytesPerSecond: normalized.RateBytesPerSecond,
		Multiplier:         normalized.Multiplier,
		JitterFraction:     normalized.JitterFraction,
		CapBytes:           normalized.CapBytes,
	})
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrLabSettingsStorageUnavailable, err)
	}
	published, err = writeFileAtomicPublished(path, append(data, '\n'))
	if err != nil {
		if published {
			return true, fmt.Errorf("%w: %v", ErrLabSettingsDurabilityUnconfirmed, err)
		}
		return false, fmt.Errorf("%w: %v", ErrLabSettingsStorageUnavailable, err)
	}
	return true, nil
}

// startLabSampler records peer activity and real payload counters into the
// announce log. It joins the session's background lifecycle, so Close cancels
// it and waits before the recorder is closed.
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
	Enabled            bool
	Settings           experiments.RuntimeSettings
	Seed               int64
	AnnounceLogPath    string
	PeerSampleInterval time.Duration
	Torrents           []LabTorrentStatus
}

// LabStatus returns the experiment configuration and the per-torrent real and
// reported upload counters. A session created without WithLabExperiment reports
// Enabled false.
func (s *Session) LabStatus() LabStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state := s.lab
	if state == nil || s.state != stateActive {
		return LabStatus{}
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
		Enabled:            true,
		Settings:           state.reporter.RuntimeSettings(),
		Seed:               state.settings.Seed,
		AnnounceLogPath:    state.settings.AnnounceLogPath,
		PeerSampleInterval: state.settings.PeerSampleInterval,
		Torrents:           torrents,
	}
}

// SetLabSettings validates, persists, and applies runtime experiment settings
// and returns the normalized value.
//
// The caller must already have validated the mode/rate combination through
// NormalizeRuntimeSettings; this method validates again so an API caller cannot
// bypass it. A session created without WithLabExperiment returns ErrLabDisabled.
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
		return experiments.RuntimeSettings{}, ErrLabDisabled
	}
	if err := ctx.Err(); err != nil {
		return experiments.RuntimeSettings{}, err
	}
	published, saveErr := saveLabSettings(s.metadataDir, normalized)
	if !published {
		s.logger.Error("lab settings save failed", "err", saveErr)
		return experiments.RuntimeSettings{}, saveErr
	}
	state.reporter.UpdateRuntime(normalized)
	if saveErr != nil {
		s.logger.Error("lab settings applied without confirmed durability", "err", saveErr)
		return normalized, saveErr
	}
	return normalized, nil
}
