package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

var (
	errInvalidPendingMagnet  = errors.New("session: invalid pending magnet")
	pendingMagnetCleanupHook func(metainfo.Hash) error
)

type managedMagnet struct {
	uri string
}

func metadataRoot(torrentsDir string) string {
	return filepath.Join(filepath.Clean(torrentsDir), ".metadata")
}

func (s *Session) finalMetainfoPath(hash metainfo.Hash) string {
	return filepath.Join(s.torrentsDir, hash.HexString()+".torrent")
}

func (s *Session) legacyMetainfoPath(hash metainfo.Hash) string {
	return filepath.Join(s.metadataDir, hash.HexString()+".torrent")
}

func (s *Session) pendingMagnetPath(hash metainfo.Hash) string {
	return filepath.Join(s.metadataDir, "pending", hash.HexString()+".magnet")
}

func (s *Session) legacyMagnetPath(hash metainfo.Hash) string {
	return filepath.Join(s.metadataDir, hash.HexString()+".magnet")
}

func (s *Session) layoutVersionPath() string {
	return filepath.Join(s.metadataDir, "layout_version")
}

func finalMetainfoName(hash metainfo.Hash) string {
	return hash.HexString() + ".torrent"
}

func pendingMagnetName(hash metainfo.Hash) string {
	return hash.HexString() + ".magnet"
}

func legacyHashFromName(name, suffix string) (metainfo.Hash, bool) {
	if !strings.HasSuffix(name, suffix) {
		return metainfo.Hash{}, false
	}
	raw := strings.TrimSuffix(name, suffix)
	hash, err := parseInfoHash(raw)
	if err != nil || raw != hash.HexString() {
		return metainfo.Hash{}, false
	}
	return hash, true
}

func ensureDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, 0o755); err != nil {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("path %q must be a directory, not a symlink", path)
	}
	return nil
}

func (s *Session) validateLayoutDirectories() error {
	if err := ensureDirectory(s.metadataDir); err != nil {
		return fmt.Errorf("session: validate metadata dir: %w", err)
	}
	if err := ensureDirectory(filepath.Join(s.metadataDir, "pending")); err != nil {
		return fmt.Errorf("session: validate pending dir: %w", err)
	}
	if err := ensureDirectory(s.stateDir); err != nil {
		return fmt.Errorf("session: validate state dir: %w", err)
	}
	return nil
}

func specName(spec *torrent.TorrentSpec) string {
	if spec == nil {
		return ""
	}
	if spec.DisplayName != "" {
		return spec.DisplayName
	}
	if len(spec.InfoBytes) == 0 {
		return ""
	}
	mi, err := metainfo.Load(bytes.NewReader(spec.InfoBytes))
	if err != nil {
		return ""
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return ""
	}
	return info.Name
}

func loadMetainfoFile(path string, expected metainfo.Hash) (*torrent.TorrentSpec, error) {
	if _, err := requireRegularFile(path); err != nil {
		return nil, err
	}
	mi, err := metainfo.LoadFromFile(path)
	if err != nil {
		return nil, err
	}
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		return nil, err
	}
	if spec.InfoHash != expected {
		return nil, fmt.Errorf("metainfo info hash %s does not match expected %s", spec.InfoHash, expected)
	}
	return spec, nil
}

func readPendingMagnet(path string, expected metainfo.Hash) (string, *torrent.TorrentSpec, error) {
	if _, err := requireRegularFile(path); err != nil {
		return "", nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	uri := strings.TrimSpace(string(data))
	spec, err := specFromSource(Source{MagnetURI: uri})
	if err != nil {
		return "", nil, err
	}
	if spec.InfoHash != expected {
		return "", nil, fmt.Errorf("magnet info hash %s does not match expected %s", spec.InfoHash, expected)
	}
	return uri, spec, nil
}

func (s *Session) publishPendingMagnet(ctx context.Context, hash metainfo.Hash, uri string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("session: persist magnet %s: %w", hash, err)
	}
	uri = strings.TrimSpace(uri)
	spec, err := specFromSource(Source{MagnetURI: uri})
	if err != nil || spec.InfoHash != hash {
		return false, fmt.Errorf("%w: magnet does not match %s", ErrInvalidSource, hash)
	}
	path := s.pendingMagnetPath(hash)
	created, err := publishNoClobber(path, []byte(uri+"\n"))
	if err != nil {
		return false, fmt.Errorf("session: persist magnet %s: %w", hash, err)
	}
	if !created {
		if _, _, err := readPendingMagnet(path, hash); err != nil {
			return false, fmt.Errorf("session: pending magnet %s: %w", hash, err)
		}
	}
	return created, nil
}

func (s *Session) publishMetainfo(ctx context.Context, hash metainfo.Hash, data []byte) (bool, *torrent.TorrentSpec, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return false, nil, fmt.Errorf("session: persist metainfo %s: %w", hash, err)
	}
	spec, err := specFromSource(Source{Metainfo: data})
	if err != nil || spec.InfoHash != hash {
		return false, nil, fmt.Errorf("%w: metainfo does not match %s", ErrInvalidSource, hash)
	}
	path := s.finalMetainfoPath(hash)
	created, err := publishNoClobber(path, data)
	if err != nil {
		return false, nil, fmt.Errorf("session: persist metainfo %s: %w", hash, err)
	}
	if !created {
		if _, err := loadMetainfoFile(path, hash); err != nil {
			return false, nil, fmt.Errorf("session: existing metainfo %s: %w", hash, err)
		}
	}
	return created, spec, nil
}

func (s *Session) writeMetadataBytes(ctx context.Context, hash metainfo.Hash, data []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("session: persist metainfo %s: %w", hash, err)
	}
	s.mu.RLock()
	deleting := s.deletionPendingLocked(hash)
	s.mu.RUnlock()
	if deleting {
		return fmt.Errorf("%w: %s", ErrDeleting, hash)
	}
	if _, _, err := s.publishMetainfo(ctx, hash, data); err != nil {
		return err
	}
	if err := s.removePendingMagnet(hash); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Session) removePendingMagnet(hash metainfo.Hash) error {
	path := s.pendingMagnetPath(hash)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if _, _, err := readPendingMagnet(path, hash); err != nil {
		return fmt.Errorf("%w %s: %v", errInvalidPendingMagnet, hash, err)
	}
	if pendingMagnetCleanupHook != nil {
		if err := pendingMagnetCleanupHook(hash); err != nil {
			return err
		}
	}
	return removeRegularFile(path)
}

func (s *Session) removeFinalMetainfo(hash metainfo.Hash) error {
	path := s.finalMetainfoPath(hash)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := loadMetainfoFile(path, hash); err != nil {
		return fmt.Errorf("session: validate metainfo %s: %w", hash, err)
	}
	return removeRegularFile(path)
}

func (s *Session) updateReadyState(hash metainfo.Hash, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := cloneRegistryEntry(s.states[hash])
	if entry == nil {
		return fmt.Errorf("session: missing registry entry for %s", hash)
	}
	entry.State = StateReady
	entry.Name = name
	entry.Error = ""
	entry.OperationID = ""
	if err := s.writeRegistryEntryLocked(entry); err != nil {
		return err
	}
	s.states[hash] = entry
	return nil
}

func (s *Session) updateAddingState(hash metainfo.Hash, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := cloneRegistryEntry(s.states[hash])
	if entry == nil {
		return fmt.Errorf("session: missing registry entry for %s", hash)
	}
	entry.State = StateAdding
	entry.Name = name
	entry.Error = ""
	entry.OperationID = ""
	if err := s.writeRegistryEntryLocked(entry); err != nil {
		return err
	}
	s.states[hash] = entry
	return nil
}

func (s *Session) recordMetadataFetchFailure(hash metainfo.Hash, err error) {
	s.logger.Warn("metadata fetch failed", "hash", hash.HexString(), "err", err)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := cloneRegistryEntry(s.states[hash])
	if entry == nil || entry.State == StateDeleting || entry.State == StateDeleteFailed {
		return
	}
	entry.State = StateError
	entry.Error = err.Error()
	if writeErr := s.writeRegistryEntryLocked(entry); writeErr != nil {
		s.logger.Error("persist metadata fetch failure", "hash", hash.HexString(), "err", writeErr)
		return
	}
	s.states[hash] = entry
}

func (s *Session) restoreRegistryEntries(ctx context.Context) error {
	s.mu.RLock()
	hashes := make([]metainfo.Hash, 0, len(s.states))
	for hash := range s.states {
		hashes = append(hashes, hash)
	}
	s.mu.RUnlock()
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].HexString() < hashes[j].HexString() })

	for _, hash := range hashes {
		s.mu.RLock()
		entry := cloneRegistryEntry(s.states[hash])
		s.mu.RUnlock()
		if entry == nil || entry.State == StateDeleting || entry.State == StateDeleteFailed {
			continue
		}

		finalPath := s.finalMetainfoPath(hash)
		_, finalErr := os.Lstat(finalPath)
		finalExists := finalErr == nil
		if finalErr != nil && !errors.Is(finalErr, os.ErrNotExist) {
			return fmt.Errorf("session: inspect metainfo %s: %w", hash, finalErr)
		}
		pendingPath := s.pendingMagnetPath(hash)
		_, pendingErr := os.Lstat(pendingPath)
		pendingExists := pendingErr == nil
		if pendingErr != nil && !errors.Is(pendingErr, os.ErrNotExist) {
			return fmt.Errorf("session: inspect pending magnet %s: %w", hash, pendingErr)
		}
		if entry.State == StateReady && !finalExists {
			return fmt.Errorf("session: ready registry %s has no final metainfo", hash)
		}
		if entry.State != StateReady && !finalExists && !pendingExists {
			return fmt.Errorf("session: registry %s has neither final metainfo nor pending magnet", hash)
		}

		switch {
		case finalExists:
			spec, err := loadMetainfoFile(finalPath, hash)
			if err != nil {
				return fmt.Errorf("session: restore metainfo %s: %w", hash, err)
			}
			if pendingExists {
				if _, _, err := readPendingMagnet(pendingPath, hash); err != nil {
					return fmt.Errorf("session: restore pending magnet %s: %w", hash, err)
				}
			}
			s.mu.Lock()
			st, _, err := s.addTorrentSpecLocked(ctx, spec)
			s.mu.Unlock()
			if err != nil {
				return fmt.Errorf("session: restore torrent %s: %w", hash, err)
			}
			if entry.State != StateReady || entry.Name != st.Name() || entry.Error != "" || entry.OperationID != "" {
				if err := s.updateReadyState(hash, st.Name()); err != nil {
					return err
				}
			}
			if pendingExists {
				if err := s.removePendingMagnet(hash); err != nil {
					if errors.Is(err, errInvalidPendingMagnet) {
						return err
					}
					s.logger.Warn("stale pending magnet cleanup failed", "hash", hash.HexString(), "err", err)
				}
			}
		case pendingExists:
			_, spec, err := readPendingMagnet(pendingPath, hash)
			if err != nil {
				return fmt.Errorf("session: restore pending magnet %s: %w", hash, err)
			}
			s.mu.Lock()
			st, _, err := s.addTorrentSpecLocked(ctx, spec)
			s.mu.Unlock()
			if err != nil {
				return fmt.Errorf("session: restore magnet %s: %w", hash, err)
			}
			if err := s.updateAddingState(hash, firstNonEmpty(entry.Name, st.Name(), spec.DisplayName)); err != nil {
				return err
			}
			s.startMetadataFetch(hash, st)
		default:
			return fmt.Errorf("session: registry %s has neither final metainfo nor pending magnet", hash)
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

type legacyLayoutEntry struct {
	hash       metainfo.Hash
	legacyFile string
	targetFile string
	kind       string
	spec       *torrent.TorrentSpec
	uri        string
	modTime    time.Time
}

func (s *Session) migrateLegacyLayoutOnce() error {
	markerPath := s.layoutVersionPath()
	markerInfo, err := os.Lstat(markerPath)
	if err == nil {
		if markerInfo.Mode()&os.ModeSymlink != 0 || !markerInfo.Mode().IsRegular() {
			return fmt.Errorf("session: layout version %q must be a regular, non-symlink file", markerPath)
		}
		marker, err := os.ReadFile(markerPath)
		if err != nil {
			return fmt.Errorf("session: read layout version: %w", err)
		}
		if strings.TrimSpace(string(marker)) != layoutVersion {
			return fmt.Errorf("session: unsupported layout version %q", strings.TrimSpace(string(marker)))
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session: read layout version: %w", err)
	}

	entries, err := os.ReadDir(s.metadataDir)
	if err != nil {
		return fmt.Errorf("session: scan legacy metadata: %w", err)
	}
	legacy := make(map[metainfo.Hash]*legacyLayoutEntry)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".torrent") || strings.HasSuffix(name, ".magnet") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(s.metadataDir, name)
		info, err := requireRegularFile(path)
		if err != nil {
			return fmt.Errorf("session: inspect legacy metadata %q: %w", name, err)
		}
		if hash, ok := legacyHashFromName(name, ".torrent"); ok {
			spec, err := loadMetainfoFile(path, hash)
			if err != nil {
				return fmt.Errorf("session: read legacy metainfo %q: %w", name, err)
			}
			item := legacy[hash]
			if item == nil {
				item = &legacyLayoutEntry{hash: hash}
				legacy[hash] = item
			}
			item.legacyFile = path
			item.targetFile = s.finalMetainfoPath(hash)
			item.kind = "torrent"
			item.spec = spec
			item.modTime = info.ModTime()
			continue
		}
		if hash, ok := legacyHashFromName(name, ".magnet"); ok {
			uri, spec, err := readPendingMagnet(path, hash)
			if err != nil {
				return fmt.Errorf("session: read legacy magnet %q: %w", name, err)
			}
			item := legacy[hash]
			if item == nil {
				item = &legacyLayoutEntry{hash: hash}
				legacy[hash] = item
			}
			item.legacyFile = path
			item.targetFile = s.pendingMagnetPath(hash)
			item.kind = "magnet"
			item.spec = spec
			item.uri = uri
			item.modTime = info.ModTime()
			continue
		}
		return fmt.Errorf("session: legacy metadata filename %q is not canonical", name)
	}

	hashes := make([]metainfo.Hash, 0, len(legacy))
	for hash := range legacy {
		hashes = append(hashes, hash)
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].HexString() < hashes[j].HexString() })
	for _, hash := range hashes {
		item := legacy[hash]
		if item.kind == "torrent" {
			if info, err := os.Lstat(item.targetFile); err == nil {
				if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
					return fmt.Errorf("session: target metainfo %s is not a regular file", hash)
				}
				if _, err := loadMetainfoFile(item.targetFile, hash); err != nil {
					return fmt.Errorf("session: target metainfo %s: %w", hash, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("session: inspect target metainfo %s: %w", hash, err)
			}
		}
		if item.kind == "magnet" {
			if info, err := os.Lstat(item.targetFile); err == nil {
				if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
					return fmt.Errorf("session: target pending magnet %s is not a regular file", hash)
				}
				if _, _, err := readPendingMagnet(item.targetFile, hash); err != nil {
					return fmt.Errorf("session: target pending magnet %s: %w", hash, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("session: inspect target pending magnet %s: %w", hash, err)
			}
		}
	}

	for _, hash := range hashes {
		item := legacy[hash]
		s.mu.Lock()
		entry := cloneRegistryEntry(s.states[hash])
		if entry == nil {
			state := StateAdding
			name := specName(item.spec)
			if item.kind == "torrent" {
				state = StateReady
			}
			entry = &registryEntry{
				ID:        hash.HexString(),
				InfoHash:  hash.HexString(),
				Name:      name,
				State:     state,
				CreatedAt: item.modTime.UTC(),
			}
		}
		if entry.State != StateDeleting && entry.State != StateDeleteFailed {
			if item.kind == "torrent" {
				entry.State = StateReady
				entry.Name = firstNonEmpty(specName(item.spec), entry.Name)
				entry.Error = ""
				entry.OperationID = ""
			} else if entry.State != StateReady {
				entry.State = StateAdding
				entry.Name = firstNonEmpty(entry.Name, specName(item.spec))
				entry.Error = ""
				entry.OperationID = ""
			}
		}
		if err := s.writeRegistryEntryLocked(entry); err != nil {
			s.mu.Unlock()
			return err
		}
		s.states[hash] = entry
		s.mu.Unlock()
	}

	for _, hash := range hashes {
		item := legacy[hash]
		if item.kind == "torrent" {
			if _, err := os.Lstat(item.targetFile); errors.Is(err, os.ErrNotExist) {
				if err := renameNoReplace(item.legacyFile, item.targetFile); err != nil {
					return fmt.Errorf("session: migrate metainfo %s: %w", hash, err)
				}
			} else if err == nil {
				if err := removeRegularFile(item.legacyFile); err != nil {
					return fmt.Errorf("session: remove migrated metainfo %s: %w", hash, err)
				}
			} else {
				return fmt.Errorf("session: inspect migrated metainfo %s: %w", hash, err)
			}
			if err := removeLegacyPendingIfPresent(s, hash); err != nil {
				return err
			}
			continue
		}
		if _, err := os.Lstat(item.targetFile); errors.Is(err, os.ErrNotExist) {
			if err := renameNoReplace(item.legacyFile, item.targetFile); err != nil {
				return fmt.Errorf("session: migrate magnet %s: %w", hash, err)
			}
		} else if err == nil {
			if err := removeRegularFile(item.legacyFile); err != nil {
				return fmt.Errorf("session: remove migrated magnet %s: %w", hash, err)
			}
		} else {
			return fmt.Errorf("session: inspect migrated magnet %s: %w", hash, err)
		}
	}

	if err := writeFileAtomic(s.layoutVersionPath(), []byte(layoutVersion+"\n")); err != nil {
		return fmt.Errorf("session: write layout version: %w", err)
	}
	return nil
}

func removeLegacyPendingIfPresent(s *Session, hash metainfo.Hash) error {
	legacyPath := s.legacyMagnetPath(hash)
	if _, err := os.Lstat(legacyPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if _, _, err := readPendingMagnet(legacyPath, hash); err != nil {
		return fmt.Errorf("session: validate legacy magnet %s: %w", hash, err)
	}
	if err := removeRegularFile(legacyPath); err != nil {
		return fmt.Errorf("session: remove legacy magnet %s: %w", hash, err)
	}
	pendingPath := s.pendingMagnetPath(hash)
	if _, err := os.Lstat(pendingPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if _, _, err := readPendingMagnet(pendingPath, hash); err != nil {
		return fmt.Errorf("session: validate pending magnet %s: %w", hash, err)
	}
	return removeRegularFile(pendingPath)
}
