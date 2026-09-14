package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

const torrentDirScanInterval = 100 * time.Millisecond

var errTorrentFileUnstable = errors.New("torrent file changed while reading")

type torrentFileSignature struct {
	size    int64
	modTime time.Time
	info    os.FileInfo
}

func (s torrentFileSignature) valid() bool {
	return s.info != nil
}

func (s torrentFileSignature) same(other torrentFileSignature) bool {
	if !s.valid() || !other.valid() || s.size != other.size || !s.modTime.Equal(other.modTime) {
		return false
	}
	return os.SameFile(s.info, other.info)
}

type torrentDirSource struct {
	hash metainfo.Hash

	loaded    bool
	loadedSig torrentFileSignature

	failedSig torrentFileSignature
	hasFailed bool

	pendingSig torrentFileSignature
	hasPending bool
}

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

func (s *Session) watchTorrentDir(ctx context.Context, done chan struct{}) {
	defer close(done)

	ticker := time.NewTicker(torrentDirScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.scanTorrentDir(ctx, false); err != nil {
				log.Printf("session: torrent directory sync: %v", err)
			}
		}
	}
}

func (s *Session) scanTorrentDir(ctx context.Context, startup bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		if startup {
			return fmt.Errorf("session: scan torrents directory: %w", err)
		}
		return nil
	}

	entries, err := os.ReadDir(s.torrentsDir)
	if err != nil {
		return fmt.Errorf("session: scan torrents directory %q: %w", s.torrentsDir, err)
	}

	seen := make(map[string]struct{}, len(entries))
	invalid := make([]string, 0)
	candidates := make([]torrentDirCandidate, 0, len(entries))
	var errs []error
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".torrent") {
			continue
		}
		path := filepath.Join(s.torrentsDir, entry.Name())
		seen[path] = struct{}{}

		info, err := os.Lstat(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("session: inspect torrent %q: %w", path, err))
			}
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			invalid = append(invalid, path)
			continue
		}
		candidate := torrentDirCandidate{
			path: path,
			sig:  torrentFileSignature{size: info.Size(), modTime: info.ModTime(), info: info},
		}
		if !s.shouldAttemptTorrentSource(path, candidate.sig) {
			continue
		}
		candidates = append(candidates, candidate)
	}

	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			if startup {
				return fmt.Errorf("session: scan torrents directory: %w", err)
			}
			return nil
		}
		spec, sig, err := loadStableTorrent(candidate.path, candidate.sig)
		if err != nil {
			if errors.Is(err, errTorrentFileUnstable) {
				if sig.valid() {
					s.markTorrentSourceFailure(candidate.path, sig, true)
				}
				continue
			}
			if sig.valid() {
				s.markTorrentSourceFailure(candidate.path, sig, false)
			}
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			errs = append(errs, fmt.Errorf("session: scan torrent %q: %w", candidate.path, err))
			continue
		}
		if err := s.registerTorrentSource(candidate.path, sig, spec); err != nil {
			if !startup && ctx.Err() != nil {
				return nil
			}
			s.markTorrentSourceFailure(candidate.path, sig, false)
			errs = append(errs, fmt.Errorf("session: register torrent %q: %w", candidate.path, err))
		}
	}

	if err := ctx.Err(); err != nil {
		if startup {
			return fmt.Errorf("session: scan torrents directory: %w", err)
		}
		return nil
	}

	s.mu.Lock()
	for _, path := range invalid {
		if err := s.removeDirectorySourceLocked(path); err != nil {
			errs = append(errs, fmt.Errorf("session: remove torrent source %q: %w", path, err))
		}
	}
	for path := range s.directorySource {
		if _, ok := seen[path]; ok {
			continue
		}
		if err := s.removeDirectorySourceLocked(path); err != nil {
			errs = append(errs, fmt.Errorf("session: remove torrent source %q: %w", path, err))
		}
	}
	s.mu.Unlock()

	return errors.Join(errs...)
}

type torrentDirCandidate struct {
	path string
	sig  torrentFileSignature
}

func (s *Session) shouldAttemptTorrentSource(path string, sig torrentFileSignature) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	source, ok := s.directorySource[path]
	if !ok {
		return true
	}
	if source.loaded && source.loadedSig.same(sig) {
		return false
	}
	if source.hasFailed && source.failedSig.same(sig) {
		return false
	}
	return true
}

func statTorrentFile(path string) (torrentFileSignature, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return torrentFileSignature{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return torrentFileSignature{}, errors.New("torrent source is not a regular, non-symlink file")
	}
	return torrentFileSignature{size: info.Size(), modTime: info.ModTime(), info: info}, nil
}

func loadStableTorrent(path string, expected torrentFileSignature) (*torrent.TorrentSpec, torrentFileSignature, error) {
	before, err := statTorrentFile(path)
	if err != nil {
		return nil, torrentFileSignature{}, err
	}
	if !before.same(expected) {
		return nil, before, errTorrentFileUnstable
	}

	mi, parseErr := metainfo.LoadFromFile(path)
	after, statErr := statTorrentFile(path)
	if statErr != nil {
		return nil, torrentFileSignature{}, statErr
	}
	if !before.same(after) {
		return nil, after, errTorrentFileUnstable
	}
	if parseErr != nil {
		return nil, after, parseErr
	}
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		return nil, after, err
	}
	return spec, after, nil
}

func (s *Session) markTorrentSourceFailure(path string, sig torrentFileSignature, unstable bool) {
	if !sig.valid() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != stateActive {
		return
	}
	source := s.directorySource[path]
	if unstable {
		source.pendingSig = sig
		source.hasPending = true
		source.hasFailed = false
	} else {
		source.failedSig = sig
		source.hasFailed = true
		source.hasPending = false
	}
	s.directorySource[path] = source
}

func (s *Session) registerTorrentSource(path string, sig torrentFileSignature, spec *torrent.TorrentSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureActiveLocked(); err != nil {
		return err
	}
	st, _, err := s.addTorrentSpecLocked(context.Background(), spec)
	if err != nil {
		return err
	}

	old, hadOld := s.directorySource[path]
	hash := st.InfoHash()
	if !hadOld || !old.loaded {
		s.directoryRefs[hash]++
	} else if old.hash != hash {
		s.directoryRefs[hash]++
	}
	s.directorySource[path] = torrentDirSource{
		hash:      hash,
		loaded:    true,
		loadedSig: sig,
	}
	if hadOld && old.loaded && old.hash != hash {
		return s.releaseDirectoryRefLocked(old.hash)
	}
	return nil
}

func (s *Session) removeDirectorySourceLocked(path string) error {
	source, ok := s.directorySource[path]
	if !ok {
		return nil
	}
	delete(s.directorySource, path)
	if !source.loaded {
		return nil
	}
	return s.releaseDirectoryRefLocked(source.hash)
}

func (s *Session) releaseDirectoryRefLocked(hash metainfo.Hash) error {
	if refs := s.directoryRefs[hash]; refs > 1 {
		s.directoryRefs[hash] = refs - 1
		return nil
	}
	delete(s.directoryRefs, hash)
	return s.releaseTorrentIfUnreferencedLocked(hash)
}

func (s *Session) releaseTorrentIfUnreferencedLocked(hash metainfo.Hash) error {
	if s.metadataRefs[hash] > 0 || s.directoryRefs[hash] > 0 {
		return nil
	}
	if _, ok := s.manualRefs[hash]; ok {
		return nil
	}
	st, ok := s.torrents[hash]
	if !ok {
		return nil
	}
	return s.removeTorrentLocked(hash, st)
}
