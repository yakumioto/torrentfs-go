package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

type managedMagnet struct {
	name string
	uri  string
}

type managedSources struct {
	metadata []string
	magnets  []managedMagnet
}

func validMetadataName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, "/\\\x00") &&
		filepath.Base(name) == name && strings.HasSuffix(name, ".torrent")
}

func metadataRoot(torrentsDir string) string {
	return filepath.Join(filepath.Clean(torrentsDir), ".metadata")
}

func metadataName(hash metainfo.Hash) string {
	return hash.HexString() + ".torrent"
}

func magnetName(hash metainfo.Hash) string {
	return hash.HexString() + ".magnet"
}

func magnetHashFromName(name string) (metainfo.Hash, bool) {
	if !strings.HasSuffix(name, ".magnet") {
		return metainfo.Hash{}, false
	}
	hash, err := parseInfoHash(strings.TrimSuffix(name, ".magnet"))
	if err != nil || name != magnetName(hash) {
		return metainfo.Hash{}, false
	}
	return hash, true
}

func (s *Session) metadataPath(name string) string {
	return filepath.Join(s.metadataDir, name)
}

func (s *Session) rescanMetadata() error {
	entries, err := os.ReadDir(s.metadataDir)
	if err != nil {
		return fmt.Errorf("session: scan metadata dir: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !validMetadataName(name) {
			continue
		}
		path := s.metadataPath(name)
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("session: inspect metadata %q: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}

		s.mu.Lock()
		st, _, err := s.addTorrentLockedResult(context.Background(), Source{MetainfoPath: path})
		if err == nil {
			s.recordMetadataLocked(name, st.InfoHash())
		}
		s.mu.Unlock()
		if err != nil {
			return fmt.Errorf("session: restore metadata %q: %w", name, err)
		}
	}
	return s.scanPendingMagnets()
}

func (s *Session) scanPendingMagnets() error {
	entries, err := os.ReadDir(s.metadataDir)
	if err != nil {
		return fmt.Errorf("session: scan pending magnets: %w", err)
	}
	for _, entry := range entries {
		hash, ok := magnetHashFromName(entry.Name())
		if !ok {
			continue
		}
		path := s.metadataPath(entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("session: inspect pending magnet %q: %w", entry.Name(), err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("session: read pending magnet %q: %w", entry.Name(), err)
		}
		uri := strings.TrimSpace(string(data))
		spec, err := specFromSource(Source{MagnetURI: uri})
		if err != nil || spec.InfoHash != hash {
			continue
		}

		s.mu.Lock()
		// The intent is always tracked so a later deletion removes it, even
		// when a canonical .torrent already won the crash race below.
		s.pendingMagnets[hash] = managedMagnet{name: entry.Name(), uri: uri}
		s.mu.Unlock()
	}
	return nil
}

func (s *Session) restorePendingMagnets(ctx context.Context) error {
	s.mu.RLock()
	pending := make(map[metainfo.Hash]managedMagnet, len(s.pendingMagnets))
	for hash, source := range s.pendingMagnets {
		pending[hash] = source
	}
	s.mu.RUnlock()

	hashes := make([]metainfo.Hash, 0, len(pending))
	for hash := range pending {
		hashes = append(hashes, hash)
	}
	sort.Slice(hashes, func(i, j int) bool {
		return hashes[i].HexString() < hashes[j].HexString()
	})
	for _, hash := range hashes {
		source := pending[hash]
		unlock := s.lockHash(hash)
		s.mu.Lock()
		if err := s.ensureActiveLocked(); err != nil {
			s.mu.Unlock()
			unlock()
			return err
		}
		if s.deletionPendingLocked(hash) || s.metadataRefs[hash] > 0 || s.directoryRefs[hash] > 0 {
			s.mu.Unlock()
			unlock()
			continue
		}
		st, _, err := s.addTorrentLockedResult(ctx, Source{MagnetURI: source.uri})
		if err == nil && s.states[hash] == nil {
			s.states[hash] = &registryEntry{
				ID:        hash.HexString(),
				InfoHash:  hash.HexString(),
				Name:      st.Name(),
				State:     StateAdding,
				CreatedAt: time.Now().UTC(),
			}
		}
		s.mu.Unlock()
		unlock()
		if err != nil {
			return fmt.Errorf("session: restore pending magnet %s: %w", hash, err)
		}
		s.startMetadataFetch(hash, st)
	}
	return nil
}

func (s *Session) recordMetadataLocked(name string, hash metainfo.Hash) {
	if current, ok := s.metadata[name]; ok {
		if current == hash {
			return
		}
		if refs := s.metadataRefs[current]; refs > 1 {
			s.metadataRefs[current] = refs - 1
		} else {
			delete(s.metadataRefs, current)
		}
	}
	s.metadata[name] = hash
	s.metadataRefs[hash]++
}

func (s *Session) persistMagnetIntent(ctx context.Context, hash metainfo.Hash, uri string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("session: persist magnet %s: %w", hash, err)
	}
	uri = strings.TrimSpace(uri)
	spec, err := specFromSource(Source{MagnetURI: uri})
	if err != nil || spec.InfoHash != hash {
		return fmt.Errorf("%w: magnet does not match %s", ErrInvalidSource, hash)
	}

	s.mu.RLock()
	if s.deletionPendingLocked(hash) {
		s.mu.RUnlock()
		return fmt.Errorf("%w: %s", ErrDeleting, hash)
	}
	if s.metadataRefs[hash] > 0 {
		s.mu.RUnlock()
		return nil
	}
	if _, ok := s.pendingMagnets[hash]; ok {
		s.mu.RUnlock()
		return nil
	}
	s.mu.RUnlock()

	name := magnetName(hash)
	path := s.metadataPath(name)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("session: pending magnet %q is not a regular file", name)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("session: read pending magnet %q: %w", name, err)
		}
		existing := strings.TrimSpace(string(data))
		existingSpec, err := specFromSource(Source{MagnetURI: existing})
		if err != nil || existingSpec.InfoHash != hash {
			return fmt.Errorf("session: pending magnet %q does not match its filename", name)
		}
		uri = existing
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session: inspect pending magnet %q: %w", name, err)
	} else if err := writeFileAtomic(path, []byte(uri+"\n")); err != nil {
		return fmt.Errorf("session: persist magnet %s: %w", hash, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureActiveLocked(); err != nil {
		return err
	}
	if s.deletionPendingLocked(hash) {
		return fmt.Errorf("%w: %s", ErrDeleting, hash)
	}
	if s.metadataRefs[hash] == 0 {
		s.pendingMagnets[hash] = managedMagnet{name: name, uri: uri}
	}
	return nil
}

func (s *Session) writeMetadataBytes(ctx context.Context, hash metainfo.Hash, data []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("session: persist metainfo %s: %w", hash, err)
	}
	spec, err := specFromSource(Source{Metainfo: data})
	if err != nil || spec.InfoHash != hash {
		return fmt.Errorf("%w: metainfo does not match %s", ErrInvalidSource, hash)
	}

	name := metadataName(hash)
	path := s.metadataPath(name)
	s.mu.RLock()
	if s.deletionPendingLocked(hash) {
		s.mu.RUnlock()
		return fmt.Errorf("%w: %s", ErrDeleting, hash)
	}
	if existing, ok := s.metadata[name]; ok {
		s.mu.RUnlock()
		if existing == hash {
			return nil
		}
		return fmt.Errorf("session: metadata %q has a different info hash", name)
	}
	s.mu.RUnlock()

	published := false
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("session: metadata %q is not a regular file", name)
		}
		existing, err := specFromSource(Source{MetainfoPath: path})
		if err != nil || existing.InfoHash != hash {
			return fmt.Errorf("session: metadata %q does not match its filename", name)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session: inspect metadata %q: %w", name, err)
	} else if err := writeFileAtomic(path, data); err != nil {
		return fmt.Errorf("session: persist metainfo %s: %w", hash, err)
	} else {
		published = true
	}

	s.mu.Lock()
	if err := s.ensureActiveLocked(); err != nil {
		s.mu.Unlock()
		return err
	}
	if s.deletionPendingLocked(hash) {
		s.mu.Unlock()
		if published {
			_ = os.Remove(path)
		}
		return fmt.Errorf("%w: %s", ErrDeleting, hash)
	}
	s.recordMetadataLocked(name, hash)
	// The canonical metainfo supersedes any unresolved magnet intent for the
	// same hash, so a crashed promotion cannot register the task twice.
	source, hasMagnet := s.pendingMagnets[hash]
	s.mu.Unlock()
	if hasMagnet {
		if err := os.Remove(s.metadataPath(source.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("session: remove pending magnet %q: %w", source.name, err)
		}
		s.mu.Lock()
		if current, ok := s.pendingMagnets[hash]; ok && current.name == source.name {
			delete(s.pendingMagnets, hash)
		}
		s.mu.Unlock()
	}
	return nil
}

func (s *Session) managedSourcesForHashLocked(hash metainfo.Hash) managedSources {
	var sources managedSources
	for name, current := range s.metadata {
		if current == hash {
			sources.metadata = append(sources.metadata, name)
		}
	}
	if source, ok := s.pendingMagnets[hash]; ok {
		sources.magnets = append(sources.magnets, source)
	}
	sort.Strings(sources.metadata)
	sort.Slice(sources.magnets, func(i, j int) bool {
		return sources.magnets[i].name < sources.magnets[j].name
	})
	return sources
}

func (s *Session) restoreManagedSourcesLocked(hash metainfo.Hash, sources managedSources) {
	for _, name := range sources.metadata {
		s.recordMetadataLocked(name, hash)
	}
	for _, source := range sources.magnets {
		s.pendingMagnets[hash] = source
	}
}

func (s *Session) releaseManagedSourcesLocked(hash metainfo.Hash) {
	for name, current := range s.metadata {
		if current == hash {
			delete(s.metadata, name)
		}
	}
	delete(s.metadataRefs, hash)
	delete(s.pendingMagnets, hash)
}
