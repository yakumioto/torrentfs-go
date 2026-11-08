// Package configmigrate folds retired runtime sidecars into the configuration
// file at startup.
//
// The upload limit used to live in <torrents-dir>/.metadata/upload_rate.json.
// The configuration schema is now the single place that owns it, so a start
// imports the retired value and archives the sidecar. A deployment that cannot
// write a configuration file keeps the sidecar value for that run and warns;
// the next start after a successful write-back completes the migration.
package configmigrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

const (
	// LegacyUploadRateFileName is the retired settings file below the
	// torrents directory's .metadata directory.
	LegacyUploadRateFileName = "upload_rate.json"
	// MigratedSuffix is appended to a sidecar that has been folded into the
	// configuration file.
	MigratedSuffix = ".migrated"
	// legacyUploadRateVersion is the only sidecar schema the migration reads.
	legacyUploadRateVersion = 1
)

// uploadRateKeys are the configuration keys the legacy sidecar maps onto, in
// file order.
var uploadRateKeys = []string{
	"upload_rate.limit",
	"upload_rate.schedule_start",
	"upload_rate.schedule_end",
}

// Options is one startup migration of the retired upload-rate sidecar.
type Options struct {
	// ConfigPath is the configuration file the process loaded. Empty means
	// the process started without one.
	ConfigPath string
	// TorrentsDir is the directory whose .metadata holds the sidecar.
	TorrentsDir string
	// Config and Sources are the effective configuration and the source of
	// every value, as the loaders produced them.
	Config  config.Config
	Sources config.Sources
	// Logger receives the migration warning or record.
	Logger *slog.Logger
	// WriteBackFile replaces the configuration file. Production leaves it
	// nil and uses config.WriteBackFile; a test injects a failure at the
	// storage boundary.
	WriteBackFile func(path string, document []byte) (published bool, err error)
}

// Result is the configuration a start should use after the migration.
type Result struct {
	// Config and Sources are the effective configuration with the legacy
	// runtime values applied when the file could not be written.
	Config  config.Config
	Sources config.Sources
	// Migrated reports that the sidecar was folded into the configuration
	// file and archived.
	Migrated bool
	// LegacyInUse reports that the sidecar value is the runtime start value
	// because the configuration file cannot be written. The sidecar remains
	// on disk; the next start after a successful write-back archives it.
	LegacyInUse bool
}

// MigrateUploadRate folds the retired upload-rate sidecar into the
// configuration file.
//
// A missing sidecar is the common case and does nothing. A writable
// configuration file receives only the keys it does not already carry, so a
// value the operator wrote by hand wins; the sidecar is archived as
// upload_rate.json.migrated. A read-only configuration file, or a deployment
// without one, keeps the sidecar value as the runtime start value and warns:
// the configuration page is already read-only in both cases, and the next
// start after a successful write-back completes the archive.
func MigrateUploadRate(options Options) (Result, error) {
	result := Result{Config: options.Config, Sources: options.Sources}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	sidecar := filepath.Join(options.TorrentsDir, ".metadata", LegacyUploadRateFileName)

	settings, found, err := readLegacyUploadRateSettings(sidecar)
	if err != nil {
		return Result{}, err
	}
	if !found {
		return result, nil
	}

	if options.ConfigPath != "" {
		exists, writable, writableErr := config.WriteBackAvailable(options.ConfigPath)
		if writableErr != nil {
			return Result{}, fmt.Errorf("configmigrate: inspect the configuration file: %w", writableErr)
		}
		if exists && writable {
			return migrateIntoConfigurationFile(options, result, settings, sidecar, logger)
		}
	}

	// Without a writable configuration file the sidecar value runs this time.
	// The page is read-only by the existing permission rules, so the value
	// cannot be saved from there either.
	if err := applyLegacyRuntime(&result, settings); err != nil {
		return Result{}, err
	}
	result.LegacyInUse = true
	reason := "the configuration file is read-only"
	if options.ConfigPath == "" {
		reason = "the process started without a configuration file"
	}
	logger.Warn("upload rate settings are not migrated: "+reason+"; using the retired .metadata/upload_rate.json for this run",
		"path", sidecar, "config_file", options.ConfigPath)
	return result, nil
}

// migrateIntoConfigurationFile writes the missing keys into the configuration
// file, archives the sidecar, and reports the configuration the running
// process should use.
func migrateIntoConfigurationFile(options Options, result Result, settings session.UploadRateSettings, sidecar string, logger *slog.Logger) (Result, error) {
	content, err := os.ReadFile(options.ConfigPath)
	if err != nil {
		return fallbackToLegacy(options, result, settings, sidecar, logger,
			"the configuration file could not be read", err)
	}
	fileConfig, fileSources, err := config.LoadContentWithSourcesFromEnvironment(content, options.ConfigPath, emptyEnvironment{})
	if err != nil {
		return Result{}, fmt.Errorf("configmigrate: load the file configuration: %w", err)
	}

	filePatch := legacyFilePatch(settings)
	var missing []string
	for _, path := range uploadRateKeys {
		if _, ok := filePatch[path]; !ok {
			continue
		}
		if fileSources.FromFile(path) {
			// The configuration file already carries the key: a value the
			// operator wrote by hand wins over the retired sidecar.
			continue
		}
		missing = append(missing, path)
	}

	if len(missing) > 0 {
		candidate := fileConfig
		for _, path := range missing {
			if err := config.SetText(&candidate, path, filePatch[path]); err != nil {
				return Result{}, fmt.Errorf("configmigrate: apply the retired value of %s: %w", path, err)
			}
		}
		document, err := config.WriteBack{File: candidate, Sources: fileSources, Changed: missing}.TOML()
		if err != nil {
			return Result{}, fmt.Errorf("configmigrate: render the configuration file: %w", err)
		}
		// Validate the file layer through the same loader a start uses. The
		// imported window and limit have already been validated together, so
		// this only catches a conflict with the rest of the file.
		if _, _, err := config.LoadContentWithSourcesFromEnvironment(document, options.ConfigPath, emptyEnvironment{}); err != nil {
			return Result{}, fmt.Errorf("configmigrate: the migrated configuration is invalid: %w", err)
		}

		writeBackFile := options.WriteBackFile
		if writeBackFile == nil {
			writeBackFile = config.WriteBackFile
		}
		published, writeErr := writeBackFile(options.ConfigPath, document)
		if !published {
			if writeErr == nil {
				writeErr = errors.New("the write-back did not publish the configuration file")
			}
			return fallbackToLegacy(options, result, settings, sidecar, logger,
				"the configuration file could not be written", writeErr)
		}
		if writeErr != nil {
			logger.Warn("upload rate settings were written back without confirmed durability",
				"path", options.ConfigPath, "err", writeErr)
		}

		// The rename committed, so the running process adopts the values it
		// just wrote. An environment override still wins: the loader gave the
		// file value precedence below it.
		if result.Sources == nil {
			result.Sources = config.Sources{}
		}
		for _, path := range missing {
			if result.Sources.SourceOf(path) == config.SourceEnvironment {
				continue
			}
			if err := config.SetText(&result.Config, path, filePatch[path]); err != nil {
				return Result{}, fmt.Errorf("configmigrate: apply the migrated value of %s: %w", path, err)
			}
			result.Sources[path] = config.SourceFile
		}
	}

	// Archive only after the file carries the value: a failure before this
	// point leaves the sidecar for the next start.
	if err := archiveSidecar(sidecar); err != nil {
		return fallbackToLegacy(options, result, settings, sidecar, logger,
			"the retired sidecar could not be archived", err)
	}
	result.Migrated = true
	logger.Info("upload rate settings migrated into the configuration file",
		"path", options.ConfigPath, "keys", missing)
	return result, nil
}

// fallbackToLegacy applies the sidecar value as the runtime start value after
// a failed file migration and leaves the sidecar in place.
func fallbackToLegacy(options Options, result Result, settings session.UploadRateSettings, sidecar string, logger *slog.Logger, reason string, cause error) (Result, error) {
	if err := applyLegacyRuntime(&result, settings); err != nil {
		return Result{}, err
	}
	result.Migrated = false
	result.LegacyInUse = true
	logger.Warn("upload rate settings are not migrated: "+reason+"; using the retired .metadata/upload_rate.json for this run",
		"path", sidecar, "config_file", options.ConfigPath, "err", cause)
	return result, nil
}

// applyLegacyRuntime overlays the sidecar value onto the runtime configuration.
//
// Every upload-rate key is overwritten unless the environment supplies it: the
// sidecar is a complete value, and an empty window must clear a window the file
// carries. The configuration page keeps reporting the file values, and it is
// read-only in every deployment that reaches this path.
func applyLegacyRuntime(result *Result, settings session.UploadRateSettings) error {
	patch := legacyRuntimePatch(settings)
	for _, path := range uploadRateKeys {
		if result.Sources.SourceOf(path) == config.SourceEnvironment {
			continue
		}
		if err := config.SetText(&result.Config, path, patch[path]); err != nil {
			return fmt.Errorf("configmigrate: apply the retired value of %s: %w", path, err)
		}
	}
	return nil
}

// legacyFilePatch returns the sidecar values a writable file should receive.
//
// Only the values the sidecar actually carries join the file: an unset limit
// or window stays absent and keeps its built-in default. A schedule implies a
// positive limit, so both are written together.
func legacyFilePatch(settings session.UploadRateSettings) map[string]string {
	patch := map[string]string{}
	if settings.RateLimitBytesPerSecond > 0 {
		patch["upload_rate.limit"] = legacyByteRate(settings.RateLimitBytesPerSecond)
	}
	if settings.Schedule != nil {
		patch["upload_rate.schedule_start"] = settings.Schedule.Start
		patch["upload_rate.schedule_end"] = settings.Schedule.End
	}
	return patch
}

// legacyRuntimePatch returns the complete upload-rate value of the sidecar:
// an unset limit and window are written as empty values so they clear a file
// value in the running configuration.
func legacyRuntimePatch(settings session.UploadRateSettings) map[string]string {
	patch := map[string]string{
		"upload_rate.limit":          "",
		"upload_rate.schedule_start": "",
		"upload_rate.schedule_end":   "",
	}
	if settings.RateLimitBytesPerSecond > 0 {
		patch["upload_rate.limit"] = legacyByteRate(settings.RateLimitBytesPerSecond)
	}
	if settings.Schedule != nil {
		patch["upload_rate.schedule_start"] = settings.Schedule.Start
		patch["upload_rate.schedule_end"] = settings.Schedule.End
	}
	return patch
}

// legacyByteRate renders a sidecar rate as the text the configuration parser
// accepts.
func legacyByteRate(rate int64) string {
	return strconv.FormatInt(rate, 10) + "B/s"
}

// archiveSidecar renames the retired sidecar so a later start sees it as
// handled. The archived file keeps the original bytes for auditing.
func archiveSidecar(path string) error {
	return os.Rename(path, path+MigratedSuffix)
}

// legacyUploadRateRecord is the on-disk schema of the retired sidecar. It is
// deliberately separate from the runtime type so the version field never
// becomes part of the public contract.
type legacyUploadRateRecord struct {
	Version                 int                       `json:"version"`
	RateLimitBytesPerSecond int64                     `json:"rate_limit_bytes_per_second"`
	Schedule                *legacyUploadRateSchedule `json:"schedule"`
}

// legacyUploadRateSchedule is the retired sidecar's window shape.
type legacyUploadRateSchedule struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// readLegacyUploadRateSettings reads the retired sidecar. A missing file means
// the deployment never used the retired setting; anything else that cannot be
// read or understood fails the start, so an operator never believes a limit is
// in force when it is not.
func readLegacyUploadRateSettings(path string) (session.UploadRateSettings, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return session.UploadRateSettings{}, false, nil
	}
	if err != nil {
		return session.UploadRateSettings{}, false, fmt.Errorf("configmigrate: inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return session.UploadRateSettings{}, false, fmt.Errorf("configmigrate: %s is not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return session.UploadRateSettings{}, false, fmt.Errorf("configmigrate: read %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var record legacyUploadRateRecord
	if err := decoder.Decode(&record); err != nil {
		return session.UploadRateSettings{}, false, fmt.Errorf("configmigrate: decode %s: %w", path, err)
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return session.UploadRateSettings{}, false, fmt.Errorf("configmigrate: decode %s: unexpected trailing data", path)
	}
	if record.Version != legacyUploadRateVersion {
		return session.UploadRateSettings{}, false, fmt.Errorf("configmigrate: %s: unsupported version %d", path, record.Version)
	}
	settings := session.UploadRateSettings{RateLimitBytesPerSecond: record.RateLimitBytesPerSecond}
	if record.Schedule != nil {
		settings.Schedule = &session.UploadRateSchedule{Start: record.Schedule.Start, End: record.Schedule.End}
	}
	normalized, err := session.NormalizeUploadRateSettings(settings)
	if err != nil {
		return session.UploadRateSettings{}, false, fmt.Errorf("configmigrate: %s: %w", path, err)
	}
	return normalized, true, nil
}

// emptyEnvironment is an environment with no variable set. The file layer is
// loaded through the same loader a start uses, with the environment left out.
type emptyEnvironment struct{}

// Lookup reports that no environment variable is set.
func (emptyEnvironment) Lookup(string) (string, bool) { return "", false }

// Names reports no environment variable.
func (emptyEnvironment) Names() []string { return nil }
