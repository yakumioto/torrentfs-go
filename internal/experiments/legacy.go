package experiments

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// The upload statistics experiment once persisted its runtime settings in a
// .metadata/lab_settings.json sidecar next to the torrents directory. The
// sidecar is retired: the configuration file is the only source of the
// settings. This file keeps the one-time reader the startup migration uses and
// nothing writes the sidecar any more.

const (
	legacySettingsFileName = "lab_settings.json"
	legacySettingsVersion  = 1
)

// legacySettingsRecord is the on-disk schema of the retired sidecar.
type legacySettingsRecord struct {
	Version            int     `json:"version"`
	Mode               Mode    `json:"mode"`
	RateBytesPerSecond float64 `json:"rate_bytes_per_second"`
	Multiplier         float64 `json:"multiplier"`
	JitterFraction     float64 `json:"jitter_fraction"`
	CapBytes           int64   `json:"cap_bytes"`
}

// LegacySettingsPath returns the path of the retired settings sidecar of a
// metadata directory.
func LegacySettingsPath(metadataDir string) string {
	return filepath.Join(metadataDir, legacySettingsFileName)
}

// LoadLegacySettings reads the retired sidecar. A missing file reports
// found=false with no error. Anything else that cannot be read or understood
// is an error: silently dropping the operator's experiment settings would
// start the process with a different experiment than the operator configured.
func LoadLegacySettings(metadataDir string) (settings RuntimeSettings, found bool, err error) {
	path := LegacySettingsPath(metadataDir)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return RuntimeSettings{}, false, nil
	}
	if err != nil {
		return RuntimeSettings{}, false, fmt.Errorf("experiments: inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return RuntimeSettings{}, false, fmt.Errorf("experiments: %s must be a regular, non-symlink file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return RuntimeSettings{}, false, fmt.Errorf("experiments: read %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var record legacySettingsRecord
	if err := decoder.Decode(&record); err != nil {
		return RuntimeSettings{}, false, fmt.Errorf("experiments: decode %s: %w", path, err)
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return RuntimeSettings{}, false, fmt.Errorf("experiments: decode %s: unexpected trailing data", path)
	}
	if record.Version != legacySettingsVersion {
		return RuntimeSettings{}, false, fmt.Errorf("experiments: %s: unsupported version %d", path, record.Version)
	}
	normalized, err := NormalizeRuntimeSettings(RuntimeSettings{
		Mode:               record.Mode,
		RateBytesPerSecond: record.RateBytesPerSecond,
		Multiplier:         record.Multiplier,
		JitterFraction:     record.JitterFraction,
		CapBytes:           record.CapBytes,
	})
	if err != nil {
		return RuntimeSettings{}, false, fmt.Errorf("experiments: %s: %w", path, err)
	}
	return normalized, true, nil
}

// ArchiveLegacySettings renames the retired sidecar to
// lab_settings.json.migrated, so a later start neither reads nor migrates it
// again. It returns the archived path.
func ArchiveLegacySettings(metadataDir string) (string, error) {
	path := LegacySettingsPath(metadataDir)
	archived := path + ".migrated"
	if err := os.Rename(path, archived); err != nil {
		return "", fmt.Errorf("experiments: archive %s: %w", path, err)
	}
	return archived, nil
}
