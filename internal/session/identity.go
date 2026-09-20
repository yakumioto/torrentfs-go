package session

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// peerIDSize is the fixed BitTorrent peer ID length in bytes.
const peerIDSize = 20

// peerIDFileName is the durable peer ID inside the data directory. Without it
// every restart would look like a brand-new peer to a private tracker: the
// client picks a fresh random suffix and a fresh dynamic port each time.
const peerIDFileName = "peer_id"

// instanceLockFileName is the lock file inside <torrents-dir>/.metadata. Two
// instances sharing one torrents directory would fight over the same metadata,
// registries, and (once the peer ID is durable) the same peer identity.
const instanceLockFileName = "instance.lock"

func peerIDPath(dataDir string) string {
	return filepath.Join(dataDir, peerIDFileName)
}

// resolvePeerID returns the peer ID the client must use. An explicitly
// configured peer ID wins and is never persisted. Otherwise the ID is read from
// the data directory and generated once when missing. A stored ID whose length
// is wrong is a hard failure: silently regenerating it would reintroduce the
// identity drift this function exists to prevent.
func resolvePeerID(dataDir, configured, prefix string, logger *slog.Logger) (string, error) {
	if configured != "" {
		if len(configured) != peerIDSize {
			return "", fmt.Errorf("session: peer ID must be exactly %d bytes, got %d", peerIDSize, len(configured))
		}
		return configured, nil
	}

	path := peerIDPath(dataDir)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(data) != peerIDSize {
			return "", fmt.Errorf("session: peer ID file %q holds %d bytes, want %d; delete it to let torrentfs generate a new identity", path, len(data), peerIDSize)
		}
		if prefix == "" || strings.HasPrefix(string(data), prefix) {
			return string(data), nil
		}
		if logger != nil {
			logger.Warn("persisted peer ID does not match identity.peer_id_prefix; generating a new one",
				"path", path,
				"prefix", prefix,
			)
		}
	case !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("session: read peer ID %q: %w", path, err)
	}

	id, err := generatePeerID(prefix)
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(path, []byte(id)); err != nil {
		return "", fmt.Errorf("session: persist peer ID %q: %w", path, err)
	}
	return id, nil
}

// generatePeerID builds a 20-byte peer ID from a prefix plus random bytes,
// mirroring how the upstream client fills a short Bep20 prefix.
func generatePeerID(prefix string) (string, error) {
	if len(prefix) > peerIDSize {
		return "", fmt.Errorf("session: peer ID prefix must be at most %d bytes, got %d", peerIDSize, len(prefix))
	}
	id := make([]byte, peerIDSize)
	copy(id, prefix)
	if _, err := rand.Read(id[len(prefix):]); err != nil {
		return "", fmt.Errorf("session: generate peer ID: %w", err)
	}
	return string(id), nil
}

// lockInstance takes a non-blocking exclusive lock on metadataDir and returns
// the open file that holds it. The lock is advisory and scoped to the file
// description, so it is released when the file is closed or the process exits.
func lockInstance(metadataDir string) (*os.File, error) {
	path := filepath.Join(metadataDir, instanceLockFileName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("session: open instance lock %q: %w", path, err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("session: another torrentfs instance already manages this torrents directory (lock %q); stop it first or point this instance at a different directory", path)
		}
		return nil, fmt.Errorf("session: lock %q: %w", path, err)
	}
	return file, nil
}

// releaseInstanceLock releases the flock and closes the lock file. Release
// happens through the file close, so a process that exits without calling this
// still frees the lock.
func releaseInstanceLock(file *os.File) {
	if file == nil {
		return
	}
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
	_ = file.Close()
}
