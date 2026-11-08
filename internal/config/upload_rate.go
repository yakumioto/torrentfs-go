package config

import "errors"

// errUploadRateTime is the strict time format error shared by the
// configuration and the session runtime.
var errUploadRateTime = errors.New("must use 24-hour HH:MM")

// UploadRate configures the session-wide aggregate payload upload limit and
// the optional daily window in which it applies.
//
// The limit is the total payload upload rate across every torrent and peer,
// not a per-torrent budget. The optional window is a same-day interval in
// which the limit applies; a window that crosses midnight is not supported.
type UploadRate struct {
	// Limit is the aggregate payload upload rate across every torrent and
	// peer. The empty value disables upload limiting. A window requires a
	// positive limit.
	Limit ByteRate `toml:"limit" component:"session" hotapply:"true"`
	// ScheduleStart is the start of the daily window in strict 24-hour HH:MM
	// (local time). Both boundaries empty means the limit applies around the
	// clock. It must be set together with ScheduleEnd and must be earlier
	// than it.
	ScheduleStart string `toml:"schedule_start" component:"session" hotapply:"true"`
	// ScheduleEnd is the end of the daily window in strict 24-hour HH:MM
	// (local time), excluded from the window. Both boundaries empty means the
	// limit applies around the clock.
	ScheduleEnd string `toml:"schedule_end" component:"session" hotapply:"true"`
}

// ParseUploadRateTime parses a strict 24-hour HH:MM window boundary into
// minutes after midnight.
//
// Only the exact five-character form is accepted, so "8:00", "08:60" and
// "24:00" are rejected rather than silently normalized. The configuration
// validator and the runtime settings validator both use this function, so a
// value the page accepts means the same thing to a start.
func ParseUploadRateTime(raw string) (int, error) {
	if len(raw) != 5 || raw[2] != ':' ||
		raw[0] < '0' || raw[0] > '9' || raw[1] < '0' || raw[1] > '9' ||
		raw[3] < '0' || raw[3] > '9' || raw[4] < '0' || raw[4] > '9' {
		return 0, errUploadRateTime
	}
	hour := int(raw[0]-'0')*10 + int(raw[1]-'0')
	minute := int(raw[3]-'0')*10 + int(raw[4]-'0')
	if hour > 23 || minute > 59 {
		return 0, errUploadRateTime
	}
	return hour*60 + minute, nil
}

// ValidateShape checks the upload limit and its optional window.
//
// The rules that relate the keys to each other live here, in the same function
// that checks a single value, so the start, the configuration page and the
// runtime mapping cannot drift apart. The session maps its runtime settings
// onto this shape and calls the same function.
func (u UploadRate) ValidateShape() error {
	uploadRate := u
	if uploadRate.Limit < 0 {
		return invalid("upload_rate.limit", errNonNegative)
	}
	start := uploadRate.ScheduleStart
	end := uploadRate.ScheduleEnd
	switch {
	case start == "" && end == "":
		return nil
	case start == "":
		return invalid("upload_rate.schedule_start",
			errors.New("must be set together with upload_rate.schedule_end"))
	case end == "":
		return invalid("upload_rate.schedule_end",
			errors.New("must be set together with upload_rate.schedule_start"))
	}
	if uploadRate.Limit <= 0 {
		return invalid("upload_rate.limit", errors.New("must be positive when a schedule window is configured"))
	}
	startMinute, err := ParseUploadRateTime(start)
	if err != nil {
		return invalid("upload_rate.schedule_start", err)
	}
	endMinute, err := ParseUploadRateTime(end)
	if err != nil {
		return invalid("upload_rate.schedule_end", err)
	}
	if startMinute >= endMinute {
		return invalid("upload_rate.schedule_start",
			errors.New("must be before upload_rate.schedule_end on the same day"))
	}
	return nil
}

// validateUploadRateShape checks the configuration values through the shared
// shape rule.
func (c Config) validateUploadRateShape() error {
	return c.UploadRate.ValidateShape()
}

// UploadRateWindowConfigured reports whether both boundaries of the daily
// window are set. The configuration validator decides whether the pair is
// usable; this reports only whether the limit is meant to be scheduled.
func (c Config) UploadRateWindowConfigured() bool {
	return c.UploadRate.ScheduleStart != "" && c.UploadRate.ScheduleEnd != ""
}
