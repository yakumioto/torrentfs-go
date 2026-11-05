package api

import (
	"context"
	"errors"
	"math"
	"net/http"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/lab"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// Stable machine-readable reasons for a rejected lab settings request. Clients
// branch on these; the messages stay display-only.
const (
	LabCodeInvalid               = "invalid_lab_settings"
	LabCodeStorageUnavailable    = "lab_settings_storage_unavailable"
	LabCodeDurabilityUnconfirmed = "lab_settings_durability_unconfirmed"
	LabCodeDisabled              = "lab_disabled"
)

// maxLabSettingsBodyBytes caps the settings request body. A settings document
// is a few dozen bytes; the limit only stops a client from streaming an
// unbounded body into the decoder.
const maxLabSettingsBodyBytes = 4 << 10

const defaultLabJitterPercent = 20

// labSettingsBody is the wire form of the runtime-tunable experiment settings.
type labSettingsBody struct {
	Mode               string  `json:"mode"`
	RateBytesPerSecond int64   `json:"rate_bytes_per_second"`
	Multiplier         float64 `json:"multiplier"`
	JitterPercent      float64 `json:"jitter_percent"`
	CapBytes           int64   `json:"cap_bytes"`
}

// labTorrentBody is the per-torrent real versus reported upload comparison.
type labTorrentBody struct {
	InfoHash              string `json:"info_hash"`
	RealUploadedBytes     int64  `json:"real_uploaded_bytes"`
	ReportedUploadedBytes int64  `json:"reported_uploaded_bytes"`
}

// labTotalsBody aggregates the tracked torrents. OverreportedBytes is
// reported minus real and may be negative.
type labTotalsBody struct {
	RealUploadedBytes     int64 `json:"real_uploaded_bytes"`
	ReportedUploadedBytes int64 `json:"reported_uploaded_bytes"`
	OverreportedBytes     int64 `json:"overreported_bytes"`
}

// labBody is the GET /api/v1/lab response contract.
type labBody struct {
	Enabled                   bool             `json:"enabled"`
	Settings                  *labSettingsBody `json:"settings"`
	Seed                      int64            `json:"seed"`
	AnnounceLog               string           `json:"announce_log"`
	PeerSampleIntervalSeconds float64          `json:"peer_sample_interval_seconds"`
	Totals                    labTotalsBody    `json:"totals"`
	Torrents                  []labTorrentBody `json:"torrents"`
}

// labSettingsRequest keeps every field pointer-valued so a missing field can
// fall back to a default instead of silently becoming zero.
type labSettingsRequest struct {
	Mode               *string  `json:"mode"`
	RateBytesPerSecond *int64   `json:"rate_bytes_per_second"`
	Multiplier         *float64 `json:"multiplier"`
	JitterPercent      *float64 `json:"jitter_percent"`
	CapBytes           *int64   `json:"cap_bytes"`
}

func newLabBody(status session.LabStatus) labBody {
	body := labBody{Torrents: []labTorrentBody{}}
	if !status.Enabled {
		return body
	}
	body.Enabled = true
	body.Seed = status.Seed
	body.AnnounceLog = status.AnnounceLogPath
	body.PeerSampleIntervalSeconds = status.PeerSampleInterval.Seconds()
	settings := status.Settings
	body.Settings = &labSettingsBody{
		Mode:               string(settings.Mode),
		RateBytesPerSecond: int64(math.Round(settings.RateBytesPerSecond)),
		Multiplier:         settings.Multiplier,
		JitterPercent:      settings.JitterFraction * 100,
		CapBytes:           settings.CapBytes,
	}
	for _, torrent := range status.Torrents {
		body.Torrents = append(body.Torrents, labTorrentBody{
			InfoHash:              torrent.InfoHash,
			RealUploadedBytes:     torrent.RealUploadedBytes,
			ReportedUploadedBytes: torrent.ReportedUploadedBytes,
		})
		body.Totals.RealUploadedBytes += torrent.RealUploadedBytes
		body.Totals.ReportedUploadedBytes += torrent.ReportedUploadedBytes
	}
	body.Totals.OverreportedBytes = body.Totals.ReportedUploadedBytes - body.Totals.RealUploadedBytes
	return body
}

func (b labSettingsRequest) toSession() session.LabSettings {
	settings := session.LabSettings{Mode: lab.Mode(*b.Mode)}
	if b.RateBytesPerSecond != nil {
		settings.RateBytesPerSecond = float64(*b.RateBytesPerSecond)
	}
	if b.Multiplier != nil {
		settings.Multiplier = *b.Multiplier
	}
	if b.JitterPercent != nil {
		settings.JitterFraction = *b.JitterPercent / 100
	} else {
		settings.JitterFraction = float64(defaultLabJitterPercent) / 100
	}
	if b.CapBytes != nil {
		settings.CapBytes = *b.CapBytes
	}
	return settings
}

func (s *Server) handleGetLab(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, newLabBody(s.backend.LabStatus()))
}

func (s *Server) handleSetLabSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxLabSettingsBodyBytes)
	var request labSettingsRequest
	if err := decodeJSON(r.Body, &request); err != nil {
		if isTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeLabSettingsFailure(w, http.StatusBadRequest, LabCodeInvalid, false, "上传实验设置格式无效")
		return
	}
	if request.Mode == nil {
		writeLabSettingsFailure(w, http.StatusBadRequest, LabCodeInvalid, false, "上传实验设置必须包含 mode")
		return
	}
	if _, err := s.backend.SetLabSettings(r.Context(), request.toSession()); err != nil {
		s.writeLabSettingsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newLabBody(s.backend.LabStatus()))
}

// writeLabSettingsError maps a session error onto the documented contract. The
// applied flag tells a client whether the running experiment already changed,
// because a durability failure happens after the new value is in force.
func (s *Server) writeLabSettingsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrInvalidLabSettings):
		writeLabSettingsFailure(w, http.StatusBadRequest, LabCodeInvalid, false, "上传实验设置无效")
	case errors.Is(err, session.ErrLabSettingsDurabilityUnconfirmed):
		s.logger.Error("lab settings durability unconfirmed", "err", err)
		writeLabSettingsFailure(w, http.StatusServiceUnavailable, LabCodeDurabilityUnconfirmed, true,
			"新设置已生效，但无法确认重启后仍能恢复")
	case errors.Is(err, session.ErrLabDisabled):
		writeLabSettingsFailure(w, http.StatusConflict, LabCodeDisabled, false,
			"上传实验未启用，请设置 TORRENTFS_LAB_* 环境变量后重启")
	case errors.Is(err, session.ErrLabSettingsStorageUnavailable),
		errors.Is(err, filesystem.ErrClosed),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		s.logger.Error("lab settings not applied", "err", err)
		writeLabSettingsFailure(w, http.StatusServiceUnavailable, LabCodeStorageUnavailable, false,
			"上传实验设置未能写入，当前设置未改变")
	default:
		s.logger.Error("lab settings update failed", "err", err)
		writeError(w, http.StatusInternalServerError, "lab settings update failed")
	}
}

func writeLabSettingsFailure(w http.ResponseWriter, status int, code string, applied bool, message string) {
	writeJSON(w, status, map[string]any{
		"error":   message,
		"code":    code,
		"applied": applied,
	})
}
