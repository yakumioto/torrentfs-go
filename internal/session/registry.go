package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

// TorrentState is the lifecycle state exposed by the management API.
type TorrentState string

const (
	StateAdding       TorrentState = "adding"
	StateDownloading  TorrentState = "downloading"
	StateSeeding      TorrentState = "seeding"
	StateError        TorrentState = "error"
	StateDeleting     TorrentState = "deleting"
	StateDeleteFailed TorrentState = "delete_failed"
	// StateDeleted is an in-memory terminal state of a deletion operation. It
	// is never written to a sidecar: a completed deletion has no sidecar left.
	StateDeleted TorrentState = "deleted"
)

// TorrentView is the management API's per-torrent snapshot.
type TorrentView struct {
	ID             string
	InfoHash       string
	Name           string
	State          TorrentState
	TotalBytes     int64
	CompletedBytes int64
	Progress       float64
	CreatedAt      time.Time
	Error          string
}

// Operation tracks one deletion request. It is returned by DeleteTorrent and
// polled through Operation.
type Operation struct {
	ID        string
	TorrentID string
	State     TorrentState
	PurgeData bool
	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (o *Operation) clone() Operation {
	return *o
}

// registryEntry is the durable per-torrent sidecar written to
// <data_dir>/state/<info_hash>.json.
type registryEntry struct {
	ID             string       `json:"id"`
	InfoHash       string       `json:"info_hash"`
	Name           string       `json:"name"`
	State          TorrentState `json:"state"`
	Error          string       `json:"error,omitempty"`
	PurgeRequested bool         `json:"purge_requested"`
	OperationID    string       `json:"operation_id,omitempty"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
}

func (s *Session) registryPath(hash metainfo.Hash) string {
	return filepath.Join(s.stateDir, hash.HexString()+".json")
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".torrentfs-state-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// writeRegistryEntryLocked durably records one torrent's state. It must be
// called with s.mu held so a deletion marker is on disk before any cleanup.
func (s *Session) writeRegistryEntryLocked(entry *registryEntry) error {
	hash, err := parseInfoHash(entry.InfoHash)
	if err != nil {
		return err
	}
	entry.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("session: encode state %s: %w", entry.InfoHash, err)
	}
	if err := writeFileAtomic(s.registryPath(hash), data); err != nil {
		return fmt.Errorf("session: write state %s: %w", entry.InfoHash, err)
	}
	return nil
}

func (s *Session) removeRegistryEntryLocked(hash metainfo.Hash) error {
	if err := os.Remove(s.registryPath(hash)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session: remove state %s: %w", hash, err)
	}
	delete(s.states, hash)
	return nil
}

// loadRegistry restores durable per-torrent state from the state directory.
func (s *Session) loadRegistry() error {
	entries, err := os.ReadDir(s.stateDir)
	if err != nil {
		return fmt.Errorf("session: scan state dir: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, dirEntry := range entries {
		name := dirEntry.Name()
		if !strings.HasSuffix(name, ".json") || dirEntry.Type()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(s.stateDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("session: read state %q: %w", name, err)
		}
		var entry registryEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			return fmt.Errorf("session: decode state %q: %w", name, err)
		}
		hash, err := parseInfoHash(entry.InfoHash)
		if err != nil {
			return fmt.Errorf("session: state %q: %w", name, err)
		}
		s.states[hash] = &entry
	}
	return nil
}

func newOperationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// parseInfoHash parses a canonical 40-character hex info hash.
func parseInfoHash(raw string) (metainfo.Hash, error) {
	if len(raw) != 40 {
		return metainfo.Hash{}, fmt.Errorf("session: invalid info hash %q", raw)
	}
	b, err := hex.DecodeString(strings.ToLower(raw))
	if err != nil || len(b) != metainfo.HashSize {
		return metainfo.Hash{}, fmt.Errorf("session: invalid info hash %q", raw)
	}
	var hash metainfo.Hash
	copy(hash[:], b)
	return hash, nil
}
