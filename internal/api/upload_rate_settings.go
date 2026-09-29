package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// Stable machine-readable reasons for a rejected upload-rate settings request.
// Clients branch on these; the messages stay display-only.
const (
	UploadRateSettingsCodeInvalid               = "invalid_upload_rate_settings"
	UploadRateSettingsCodeStorageUnavailable    = "upload_rate_settings_storage_unavailable"
	UploadRateSettingsCodeDurabilityUnconfirmed = "upload_rate_settings_durability_unconfirmed"
)

// maxUploadRateSettingsBodyBytes caps the settings request body. A settings
// document is a few dozen bytes; the limit only stops a client from streaming an
// unbounded body into the decoder.
const maxUploadRateSettingsBodyBytes = 4 << 10

// uploadRateScheduleBody is the wire form of one upload window.
type uploadRateScheduleBody struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// uploadRateSettingsBody is the wire response contract. The stored schema's
// version field is deliberately absent so it never becomes part of the public
// contract.
type uploadRateSettingsBody struct {
	RateLimitBytesPerSecond int64                   `json:"rate_limit_bytes_per_second"`
	Schedule                *uploadRateScheduleBody `json:"schedule"`
}

// uploadRateSettingsRequest keeps the rate pointer-valued so a missing or null
// field cannot be mistaken for an explicit request to disable limiting.
type uploadRateSettingsRequest struct {
	RateLimitBytesPerSecond *int64                  `json:"rate_limit_bytes_per_second"`
	Schedule                *uploadRateScheduleBody `json:"schedule"`
}

func newUploadRateSettingsBody(settings session.UploadRateSettings) uploadRateSettingsBody {
	body := uploadRateSettingsBody{RateLimitBytesPerSecond: settings.RateLimitBytesPerSecond}
	if settings.Schedule != nil {
		body.Schedule = &uploadRateScheduleBody{Start: settings.Schedule.Start, End: settings.Schedule.End}
	}
	return body
}

func (b uploadRateSettingsRequest) toSession() session.UploadRateSettings {
	settings := session.UploadRateSettings{RateLimitBytesPerSecond: *b.RateLimitBytesPerSecond}
	if b.Schedule != nil {
		settings.Schedule = &session.UploadRateSchedule{Start: b.Schedule.Start, End: b.Schedule.End}
	}
	return settings
}

func (s *Server) handleGetUploadRateSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, newUploadRateSettingsBody(s.backend.UploadRateSettings()))
}

func (s *Server) handleSetUploadRateSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadRateSettingsBodyBytes)
	var body uploadRateSettingsRequest
	if err := decodeJSON(r.Body, &body); err != nil {
		if isTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeUploadRateSettingsFailure(w, http.StatusBadRequest, UploadRateSettingsCodeInvalid, false,
			"上传限速设置格式无效")
		return
	}
	if body.RateLimitBytesPerSecond == nil {
		writeUploadRateSettingsFailure(w, http.StatusBadRequest, UploadRateSettingsCodeInvalid, false,
			"上传限速设置必须包含 rate_limit_bytes_per_second")
		return
	}
	applied, err := s.backend.SetUploadRateSettings(r.Context(), body.toSession())
	if err != nil {
		s.writeUploadRateSettingsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newUploadRateSettingsBody(applied))
}

// writeUploadRateSettingsError maps a session error onto the documented
// contract. The applied flag tells a client whether the running limit already
// changed, because a durability failure happens after the new value is in force
// and must not be retried as if nothing had happened.
func (s *Server) writeUploadRateSettingsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrInvalidUploadRateSettings):
		writeUploadRateSettingsFailure(w, http.StatusBadRequest, UploadRateSettingsCodeInvalid, false,
			"上传限速设置无效")
	case errors.Is(err, session.ErrUploadRateSettingsDurabilityUnconfirmed):
		s.logger.Error("upload rate settings durability unconfirmed", "err", err)
		writeUploadRateSettingsFailure(w, http.StatusServiceUnavailable, UploadRateSettingsCodeDurabilityUnconfirmed, true,
			"新设置已生效，但无法确认重启后仍能恢复")
	case errors.Is(err, session.ErrUploadRateSettingsStorageUnavailable),
		errors.Is(err, filesystem.ErrClosed),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		s.logger.Error("upload rate settings not applied", "err", err)
		writeUploadRateSettingsFailure(w, http.StatusServiceUnavailable, UploadRateSettingsCodeStorageUnavailable, false,
			"上传限速设置未能写入，当前设置未改变")
	default:
		s.logger.Error("upload rate settings update failed", "err", err)
		writeError(w, http.StatusInternalServerError, "upload rate settings update failed")
	}
}

func writeUploadRateSettingsFailure(w http.ResponseWriter, status int, code string, applied bool, message string) {
	writeJSON(w, status, map[string]any{
		"error":   message,
		"code":    code,
		"applied": applied,
	})
}
