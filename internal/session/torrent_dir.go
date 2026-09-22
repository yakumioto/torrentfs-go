package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func validateTorrentDir(path string) (string, error) {
	if path == "" {
		return "", errors.New("session: torrents directory is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("session: validate torrents directory %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("session: torrents path %q must be an existing directory", path)
	}
	return filepath.Clean(path), nil
}
