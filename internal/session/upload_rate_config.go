package session

import (
	"context"
	"fmt"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// uploadRateConfigKeys lists the configuration keys the session applies to the
// running upload limiter.
var uploadRateConfigKeys = []string{
	"upload_rate.limit",
	"upload_rate.schedule_start",
	"upload_rate.schedule_end",
}

// UploadRateConfigKeys returns the configuration keys the session can apply
// without a restart.
//
// The configuration controller registers one applier for these keys, and every
// key is marked hot-applied by the configuration schema. A copy is returned so
// a caller cannot change the registered set.
func UploadRateConfigKeys() []string {
	keys := make([]string, len(uploadRateConfigKeys))
	copy(keys, uploadRateConfigKeys)
	return keys
}

// UploadRateSettingsFromConfig maps the upload-rate configuration onto the
// runtime settings.
//
// The configuration validator and the runtime normalizer share the same window
// parser, so a value the page accepts means the same thing to a start and to
// the running limiter. The mapping is the only place that knows the shape of
// the configuration: the rest of the session sees runtime settings.
func UploadRateSettingsFromConfig(cfg config.Config) (UploadRateSettings, error) {
	settings := UploadRateSettings{
		RateLimitBytesPerSecond: int64(cfg.UploadRate.Limit),
	}
	if cfg.UploadRateWindowConfigured() {
		settings.Schedule = &UploadRateSchedule{
			Start: cfg.UploadRate.ScheduleStart,
			End:   cfg.UploadRate.ScheduleEnd,
		}
	}
	normalized, err := NormalizeUploadRateSettings(settings)
	if err != nil {
		return UploadRateSettings{}, fmt.Errorf("session: upload rate configuration: %w", err)
	}
	return normalized, nil
}

// ApplyUploadRateConfig applies the upload-rate keys of a validated
// configuration to the running session.
//
// It is the hot-apply entry point the configuration controller calls after a
// save and after an accepted external reload. A failure leaves the running
// limit unchanged, and the controller reports the keys as restart-required.
func (s *Session) ApplyUploadRateConfig(cfg config.Config) error {
	settings, err := UploadRateSettingsFromConfig(cfg)
	if err != nil {
		return err
	}
	if _, err := s.SetUploadRateSettings(context.Background(), settings); err != nil {
		return err
	}
	return nil
}

// UploadRateApplier applies the upload-rate configuration keys to a running
// session. It matches the configuration controller's applier contract without
// the session importing the controller package.
type UploadRateApplier struct {
	session *Session
}

// NewUploadRateApplier returns the applier of the upload-rate keys to session.
func NewUploadRateApplier(session *Session) *UploadRateApplier {
	return &UploadRateApplier{session: session}
}

// Keys returns the configuration keys the applier owns.
func (a *UploadRateApplier) Keys() []string { return UploadRateConfigKeys() }

// Apply applies the upload-rate values of cfg to the running session.
func (a *UploadRateApplier) Apply(cfg config.Config) error {
	return a.session.ApplyUploadRateConfig(cfg)
}
