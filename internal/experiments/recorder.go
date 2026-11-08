package experiments

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Record kind names written to the JSON Lines announce log.
const (
	recordTypeSettings = "settings"
	recordTypeAnnounce = "announce"
	recordTypePeers    = "peers"
)

// Recorder appends experiment ground-truth records to a JSON Lines file. It is
// safe for concurrent use and every record is written as one complete line, so
// a run can be analyzed line by line even if the process is killed.
//
// A nil recorder is valid and drops every record, which keeps the reporting
// code free of conditional logging.
type Recorder struct {
	file   *os.File
	logger *slog.Logger

	mu     sync.Mutex
	closed bool
}

// OpenRecorder opens path for appending, creating it with mode 0600 when it
// does not exist. A new settings record marks every run, so appending keeps a
// complete experiment history instead of overwriting earlier runs.
func OpenRecorder(path string, logger *slog.Logger) (*Recorder, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open announce log: %w", err)
	}
	return &Recorder{file: file, logger: logger}, nil
}

// Close stops accepting records and closes the file.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return r.file.Close()
}

// SettingsRecord starts a run and records the experiment parameters.
type SettingsRecord struct {
	Type                   string    `json:"type"`
	Time                   time.Time `json:"time"`
	Mode                   Mode      `json:"mode"`
	RateBytesPerSecond     float64   `json:"rate_bytes_per_second,omitempty"`
	Multiplier             float64   `json:"multiplier,omitempty"`
	JitterFraction         float64   `json:"jitter_fraction"`
	CapBytes               int64     `json:"cap_bytes"`
	Seed                   int64     `json:"seed"`
	PeerSampleIntervalSecs float64   `json:"peer_sample_interval_seconds"`
}

// AnnounceRecord is one HTTP(S) tracker announce observed by the director,
// captured before the reported upload is rewritten.
type AnnounceRecord struct {
	Type string    `json:"type"`
	Time time.Time `json:"time"`
	// InfoHash is the hex info hash. Peer IDs, passkeys, and the raw query
	// string are deliberately not logged.
	InfoHash string `json:"info_hash"`
	// Tracker is the announce host, without path or query.
	Tracker          string `json:"tracker"`
	Event            string `json:"event,omitempty"`
	Mode             Mode   `json:"mode"`
	RealUploaded     int64  `json:"real_uploaded"`
	RealDownloaded   int64  `json:"real_downloaded"`
	Left             int64  `json:"left"`
	RealDelta        int64  `json:"real_delta"`
	ReportedUploaded int64  `json:"reported_uploaded"`
	ReportedDelta    int64  `json:"reported_delta"`
}

// PeerSampleRecord is one periodic snapshot of a torrent's live peer gauges and
// real payload counters.
type PeerSampleRecord struct {
	Type             string    `json:"type"`
	Time             time.Time `json:"time"`
	InfoHash         string    `json:"info_hash"`
	UploadedBytes    int64     `json:"uploaded_bytes"`
	DownloadedBytes  int64     `json:"downloaded_bytes"`
	TotalPeers       int       `json:"total_peers"`
	PendingPeers     int       `json:"pending_peers"`
	ActivePeers      int       `json:"active_peers"`
	ConnectedSeeders int       `json:"connected_seeders"`
}

// WriteSettings records the experiment parameters that apply to every later
// record.
func (r *Recorder) WriteSettings(settings Settings) {
	r.writeRecord(SettingsRecord{
		Type:                   recordTypeSettings,
		Time:                   time.Now(),
		Mode:                   settings.Mode,
		RateBytesPerSecond:     settings.RateBytesPerSecond,
		Multiplier:             settings.Multiplier,
		JitterFraction:         settings.JitterFraction,
		CapBytes:               settings.CapBytes,
		Seed:                   settings.Seed,
		PeerSampleIntervalSecs: settings.PeerSampleInterval.Seconds(),
	})
}

// WriteAnnounce records one announce, including the real counter read from the
// request and the value that was reported after the mode was applied.
func (r *Recorder) WriteAnnounce(record AnnounceRecord) {
	record.Type = recordTypeAnnounce
	r.writeRecord(record)
}

// WritePeerSample records one peer-activity sample.
func (r *Recorder) WritePeerSample(record PeerSampleRecord) {
	record.Type = recordTypePeers
	r.writeRecord(record)
}

func (r *Recorder) writeRecord(record any) {
	if r == nil {
		return
	}
	payload, err := json.Marshal(record)
	if err != nil {
		r.logError("encode lab record", err)
		return
	}
	payload = append(payload, '\n')
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if _, err := r.file.Write(payload); err != nil {
		r.logError("write lab record", err)
	}
}

func (r *Recorder) logError(message string, err error) {
	if r.logger == nil {
		return
	}
	r.logger.Error(message, "err", err)
}
