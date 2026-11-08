package api

import (
	"math"
	"net/http"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

// labSettingsBody is the wire form of the runtime-tunable experiment settings.
type labSettingsBody struct {
	Mode               string  `json:"mode"`
	RateBytesPerSecond int64   `json:"rate_bytes_per_second"`
	Multiplier         float64 `json:"multiplier"`
	JitterPercent      float64 `json:"jitter_percent"`
	CapBytes           int64   `json:"cap_bytes"`
}

// labTorrentBody is the per-torrent real versus reported upload comparison.
// RealUploadedBytes is live; RealUploadedAtLastAnnounceBytes is the same counter
// when the last announce was sent, which is the value the tracker can see.
type labTorrentBody struct {
	InfoHash                        string     `json:"info_hash"`
	RealUploadedBytes               int64      `json:"real_uploaded_bytes"`
	RealUploadedAtLastAnnounceBytes int64      `json:"real_uploaded_at_last_announce_bytes"`
	ReportedUploadedBytes           int64      `json:"reported_uploaded_bytes"`
	LastAnnounceAt                  *time.Time `json:"last_announce_at"`
}

// labTotalsBody aggregates the tracked torrents. OverreportedBytes is reported
// minus the real counter at the last announce and may be negative;
// PendingRealUploadBytes is the live counter minus that same announce-time
// counter, i.e. traffic the tracker has not seen yet.
type labTotalsBody struct {
	RealUploadedBytes               int64 `json:"real_uploaded_bytes"`
	RealUploadedAtLastAnnounceBytes int64 `json:"real_uploaded_at_last_announce_bytes"`
	ReportedUploadedBytes           int64 `json:"reported_uploaded_bytes"`
	OverreportedBytes               int64 `json:"overreported_bytes"`
	PendingRealUploadBytes          int64 `json:"pending_real_upload_bytes"`
}

// labBody is the GET /api/v1/experiments/upload response contract. The
// experiment is always on, so the response always carries settings and
// counters; there is no enabled field.
type labBody struct {
	Settings                  labSettingsBody  `json:"settings"`
	Seed                      int64            `json:"seed"`
	AnnounceLog               string           `json:"announce_log"`
	PeerSampleIntervalSeconds float64          `json:"peer_sample_interval_seconds"`
	Totals                    labTotalsBody    `json:"totals"`
	Torrents                  []labTorrentBody `json:"torrents"`
}

func newLabBody(status session.LabStatus) labBody {
	settings := status.Settings
	body := labBody{
		Settings: labSettingsBody{
			Mode:               string(settings.Mode),
			RateBytesPerSecond: int64(math.Round(settings.RateBytesPerSecond)),
			Multiplier:         settings.Multiplier,
			JitterPercent:      settings.JitterFraction * 100,
			CapBytes:           settings.CapBytes,
		},
		Seed:                      status.Seed,
		AnnounceLog:               status.AnnounceLogPath,
		PeerSampleIntervalSeconds: status.PeerSampleInterval.Seconds(),
		Torrents:                  []labTorrentBody{},
	}
	for _, torrent := range status.Torrents {
		entry := labTorrentBody{
			InfoHash:                        torrent.InfoHash,
			RealUploadedBytes:               torrent.RealUploadedBytes,
			RealUploadedAtLastAnnounceBytes: torrent.RealUploadedAtLastAnnounceBytes,
			ReportedUploadedBytes:           torrent.ReportedUploadedBytes,
		}
		if !torrent.LastAnnounceAt.IsZero() {
			lastAnnounce := torrent.LastAnnounceAt
			entry.LastAnnounceAt = &lastAnnounce
		}
		body.Torrents = append(body.Torrents, entry)
		body.Totals.RealUploadedBytes += torrent.RealUploadedBytes
		body.Totals.RealUploadedAtLastAnnounceBytes += torrent.RealUploadedAtLastAnnounceBytes
		body.Totals.ReportedUploadedBytes += torrent.ReportedUploadedBytes
	}
	body.Totals.OverreportedBytes = body.Totals.ReportedUploadedBytes - body.Totals.RealUploadedAtLastAnnounceBytes
	body.Totals.PendingRealUploadBytes = body.Totals.RealUploadedBytes - body.Totals.RealUploadedAtLastAnnounceBytes
	return body
}

func (s *Server) handleGetLab(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, newLabBody(s.backend.LabStatus()))
}
