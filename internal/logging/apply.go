package logging

import (
	"log/slog"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// LevelApplier applies the log level configuration to the running logger.
//
// New returns the level controller of the handler; the applier keeps it so a
// saved level reaches the handler without rebuilding the logger. The logger
// keeps its format, its source setting and its redaction: only the threshold
// changes.
type LevelApplier struct {
	level *slog.LevelVar
}

// NewLevelApplier returns the applier of the log level to level.
func NewLevelApplier(level *slog.LevelVar) *LevelApplier {
	return &LevelApplier{level: level}
}

// Keys returns the configuration keys the applier owns.
func (a *LevelApplier) Keys() []string { return []string{"log.level"} }

// Apply sets the configured log level on the running logger.
//
// The level is parsed with the same function the startup uses, so a value the
// page saved means the same thing to a restart and to the running process. An
// unparsable level leaves the current level in force.
func (a *LevelApplier) Apply(cfg config.Config) error {
	level, err := config.ParseLogLevel(cfg.Log.Level)
	if err != nil {
		return err
	}
	a.level.Set(level)
	return nil
}
