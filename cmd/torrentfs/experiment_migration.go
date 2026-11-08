package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/configwrite"
	"github.com/yakumioto/torrentfs-go/internal/experiments"
)

// experimentMigration reports how the retired .metadata/lab_settings.json
// sidecar was handled at startup.
type experimentMigration struct {
	// Migrated reports whether the sidecar values were merged into the
	// configuration file and the sidecar archived.
	Migrated bool
	// Runtime is the legacy runtime settings when they could not be merged
	// into the configuration file. It is nil otherwise. The session starts
	// with these values so an upgrade does not change the running experiment.
	Runtime *experiments.RuntimeSettings
	// ArchivedPath is the path the sidecar was archived to, empty when it was
	// not archived.
	ArchivedPath string
}

// migrateLegacyExperimentSettings imports the retired experiment sidecar into
// the configuration file.
//
// The configuration file is the only source of the experiment settings now. A
// writable file receives the sidecar values for the keys the file does not
// name; the sidecar is then archived. A deployment without a configuration
// file, or with a file that cannot be written, keeps the legacy values as a
// runtime fallback and logs a warning: the settings keep working, but they are
// not migrated until the file can be written.
func migrateLegacyExperimentSettings(configPath, torrentsDir string, logger *slog.Logger) (experimentMigration, error) {
	metadataDir := filepath.Join(filepath.Clean(torrentsDir), ".metadata")
	settings, found, err := experiments.LoadLegacySettings(metadataDir)
	if err != nil {
		return experimentMigration{}, err
	}
	if !found {
		return experimentMigration{}, nil
	}
	sidecar := experiments.LegacySettingsPath(metadataDir)
	fallback := func(reason string, cause error) (experimentMigration, error) {
		logger.Warn("legacy experiment settings are not migrated; the running process uses them until the configuration file can be written",
			"sidecar", sidecar, "reason", reason, "err", cause)
		runtime := settings
		return experimentMigration{Runtime: &runtime}, nil
	}
	if configPath == "" {
		return fallback("no configuration file", nil)
	}
	exists, writable, err := config.WriteBackAvailable(configPath)
	if err != nil {
		return experimentMigration{}, fmt.Errorf("migrate experiment settings: %w", err)
	}
	if !exists {
		return fallback("no configuration file", nil)
	}
	if !writable {
		return fallback("configuration file is not writable", nil)
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		return experimentMigration{}, fmt.Errorf("migrate experiment settings: read %q: %w", configPath, err)
	}
	file, err := configwrite.Load(content, configPath)
	if err != nil {
		// The start loader reports the real problem with the value rules. The
		// sidecar stays in place, so a fixed configuration migrates on the
		// next start.
		return experimentMigration{}, nil
	}

	candidate := file.Config
	upload := candidate.Experiments.Upload
	changed := make([]string, 0, 5)
	if file.Sources.SourceOf("experiments.upload.mode") != config.SourceFile {
		upload.Mode = string(settings.Mode)
		changed = append(changed, "experiments.upload.mode")
	}
	if file.Sources.SourceOf("experiments.upload.rate") != config.SourceFile {
		upload.Rate = config.ByteRate(settings.RateBytesPerSecond)
		changed = append(changed, "experiments.upload.rate")
	}
	if file.Sources.SourceOf("experiments.upload.multiplier") != config.SourceFile {
		upload.Multiplier = config.Ratio(settings.Multiplier)
		changed = append(changed, "experiments.upload.multiplier")
	}
	if file.Sources.SourceOf("experiments.upload.jitter") != config.SourceFile {
		upload.Jitter = config.Percent(settings.JitterFraction * 100)
		changed = append(changed, "experiments.upload.jitter")
	}
	if file.Sources.SourceOf("experiments.upload.cap") != config.SourceFile {
		upload.Cap = config.OptionalByteQuantity(settings.CapBytes)
		changed = append(changed, "experiments.upload.cap")
	}
	candidate.Experiments.Upload = upload

	if len(changed) > 0 {
		prepared, err := configwrite.Render(candidate, file.Sources, changed, configPath)
		if err != nil {
			return fallback("the merged configuration is invalid", err)
		}
		published, writeErr := configwrite.Publish(configPath, prepared.Document, nil)
		if !published {
			return fallback("the configuration file could not be written", writeErr)
		}
		if writeErr != nil {
			logger.Warn("legacy experiment settings were written without confirmed durability", "err", writeErr)
		}
	}

	archived, err := experiments.ArchiveLegacySettings(metadataDir)
	if err != nil {
		logger.Warn("legacy experiment settings were migrated but could not be archived", "err", err)
		return experimentMigration{Migrated: true}, nil
	}
	logger.Info("legacy experiment settings migrated into the configuration file",
		"keys", strings.Join(changed, ","), "archived", archived)
	return experimentMigration{Migrated: true, ArchivedPath: archived}, nil
}
