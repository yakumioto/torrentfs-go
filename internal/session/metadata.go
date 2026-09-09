package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

func validMetadataName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, "/\\\x00") &&
		filepath.Base(name) == name && strings.HasSuffix(name, ".torrent")
}

func metadataRoot(dataDir string) string {
	return filepath.Clean(dataDir) + ".metadata"
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
			hash := st.InfoHash()
			s.metadata[name] = hash
			s.metadataRefs[hash]++
		}
		s.mu.Unlock()
		if err != nil {
			return fmt.Errorf("session: restore metadata %q: %w", name, err)
		}
	}
	return nil
}

// MetadataDirExists reports whether the control directory is present.
func (s *Session) MetadataDirExists() bool {
	info, err := os.Lstat(s.metadataDir)
	return err == nil && info.Mode().IsDir()
}

// MetadataFiles returns the current regular .torrent files in metadata/ in
// filename order. Temporary commit files and symlinks are never exposed.
func (s *Session) MetadataFiles() []filesystem.MetadataView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state != stateActive {
		return nil
	}
	entries, err := os.ReadDir(s.metadataDir)
	if err != nil {
		return nil
	}
	views := make([]filesystem.MetadataView, 0, len(entries))
	for _, entry := range entries {
		if !validMetadataName(entry.Name()) || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		views = append(views, filesystem.MetadataView{Name: entry.Name(), Size: info.Size()})
	}
	return views
}

// OpenMetadata opens one regular metadata file for reading.
func (s *Session) OpenMetadata(name string) (filesystem.MetadataReader, error) {
	if !validMetadataName(name) {
		return nil, fmt.Errorf("session: metadata name %q: %w", name, filesystem.ErrInvalidName)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.ensureActiveLocked(); err != nil {
		return nil, err
	}
	path := s.metadataPath(name)
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("session: metadata %q: %w", name, filesystem.ErrNotFound)
		}
		return nil, fmt.Errorf("session: stat metadata %q: %w", name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("session: metadata %q is a symlink: %w", name, filesystem.ErrPermission)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("session: metadata %q: %w", name, filesystem.ErrIsDir)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("session: open metadata %q: %w", name, err)
	}
	return f, nil
}

// BeginMetadata reserves name and opens a private temporary file. The final
// metadata file becomes visible only after Commit validates and registers it.
func (s *Session) BeginMetadata(ctx context.Context, name string, flags uint32) (filesystem.MetadataWriter, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !validMetadataName(name) {
		return nil, fmt.Errorf("session: metadata name %q: %w", name, filesystem.ErrInvalidName)
	}
	if flags&syscall.O_ACCMODE == syscall.O_RDONLY {
		return nil, fmt.Errorf("session: metadata %q: %w", name, filesystem.ErrPermission)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("session: begin metadata %q: %w", name, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureActiveLocked(); err != nil {
		return nil, err
	}
	if _, ok := s.pendingWriters[name]; ok {
		return nil, fmt.Errorf("session: metadata %q: %w", name, filesystem.ErrExists)
	}
	if _, err := os.Lstat(s.metadataPath(name)); err == nil {
		return nil, fmt.Errorf("session: metadata %q: %w", name, filesystem.ErrExists)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("session: stat metadata %q: %w", name, err)
	}

	f, err := os.CreateTemp(s.metadataDir, ".torrentfs-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("session: create metadata temporary file: %w", err)
	}
	w := &metadataWriter{
		s:        s,
		name:     name,
		tempPath: f.Name(),
		file:     f,
		ctx:      ctx,
	}
	s.pendingWriters[name] = w
	return w, nil
}

type metadataWriter struct {
	s        *Session
	name     string
	tempPath string
	ctx      context.Context

	mu        sync.Mutex
	file      *os.File
	committed bool
	aborted   bool
}

var _ filesystem.MetadataWriter = (*metadataWriter)(nil)

func (w *metadataWriter) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, filesystem.ErrInvalidName
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed || w.aborted || w.file == nil {
		return 0, filesystem.ErrClosed
	}
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.file.WriteAt(p, off)
	if err != nil {
		return n, fmt.Errorf("session: write metadata %q: %w", w.name, err)
	}
	return n, nil
}

func (w *metadataWriter) cleanupLocked() error {
	var errs []error
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			errs = append(errs, err)
		}
		w.file = nil
	}
	if err := os.Remove(w.tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	w.s.mu.Lock()
	if w.s.pendingWriters[w.name] == w {
		delete(w.s.pendingWriters, w.name)
	}
	w.s.mu.Unlock()
	w.aborted = true
	return errors.Join(errs...)
}

func (w *metadataWriter) failLocked(err error) error {
	return errors.Join(err, w.cleanupLocked())
}

func (w *metadataWriter) Commit() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed {
		return nil
	}
	if w.aborted {
		return filesystem.ErrClosed
	}
	if err := w.ctx.Err(); err != nil {
		return w.failLocked(fmt.Errorf("session: commit metadata %q: %w", w.name, err))
	}
	if w.file != nil {
		if err := w.file.Sync(); err != nil {
			return w.failLocked(fmt.Errorf("session: sync metadata %q: %w", w.name, err))
		}
		if err := w.file.Close(); err != nil {
			w.file = nil
			return w.failLocked(fmt.Errorf("session: close metadata %q: %w", w.name, err))
		}
		w.file = nil
	}

	w.s.mu.Lock()
	if err := w.s.ensureActiveLocked(); err != nil {
		w.s.mu.Unlock()
		return w.failLocked(err)
	}
	if w.s.pendingWriters[w.name] != w {
		w.s.mu.Unlock()
		return w.failLocked(fmt.Errorf("session: metadata %q: %w", w.name, filesystem.ErrClosed))
	}
	if _, err := os.Lstat(w.s.metadataPath(w.name)); err == nil {
		w.s.mu.Unlock()
		return w.failLocked(fmt.Errorf("session: metadata %q: %w", w.name, filesystem.ErrExists))
	} else if !errors.Is(err, os.ErrNotExist) {
		w.s.mu.Unlock()
		return w.failLocked(fmt.Errorf("session: stat metadata %q: %w", w.name, err))
	}

	st, added, err := w.s.addTorrentLockedResult(w.ctx, Source{MetainfoPath: w.tempPath})
	if err != nil {
		w.s.mu.Unlock()
		return w.failLocked(err)
	}
	hash := st.InfoHash()
	if err := os.Rename(w.tempPath, w.s.metadataPath(w.name)); err != nil {
		if added {
			_ = w.s.removeTorrentLocked(hash, st)
		}
		w.s.mu.Unlock()
		return w.failLocked(fmt.Errorf("session: publish metadata %q: %w", w.name, err))
	}
	w.s.metadata[w.name] = hash
	w.s.metadataRefs[hash]++
	delete(w.s.pendingWriters, w.name)
	w.committed = true
	w.s.mu.Unlock()
	return nil
}

func (w *metadataWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed || w.aborted {
		return nil
	}
	return w.cleanupLocked()
}

// Remove removes a metadata file and drops its torrent when no other metadata
// file refers to the same info hash.
func (s *Session) Remove(name string) error {
	return s.removeMetadata(context.Background(), name)
}

func (s *Session) RemoveMetadata(ctx context.Context, name string) error {
	return s.removeMetadata(ctx, name)
}

func (s *Session) removeMetadata(ctx context.Context, name string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !validMetadataName(name) {
		return fmt.Errorf("session: metadata name %q: %w", name, filesystem.ErrInvalidName)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("session: remove metadata %q: %w", name, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureActiveLocked(); err != nil {
		return err
	}
	path := s.metadataPath(name)
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("session: metadata %q: %w", name, filesystem.ErrNotFound)
		}
		return fmt.Errorf("session: stat metadata %q: %w", name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("session: metadata %q is a symlink: %w", name, filesystem.ErrPermission)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("session: metadata %q: %w", name, filesystem.ErrIsDir)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("session: remove metadata %q: %w", name, err)
	}

	hash, registered := s.metadata[name]
	delete(s.metadata, name)
	if !registered {
		return nil
	}
	s.metadataRefs[hash]--
	if s.metadataRefs[hash] > 0 {
		return nil
	}
	delete(s.metadataRefs, hash)
	st, ok := s.torrents[hash]
	if !ok {
		return nil
	}
	return s.removeTorrentLocked(hash, st)
}

func (s *Session) removeTorrentLocked(hash metainfo.Hash, st *Torrent) error {
	if current, ok := s.torrents[hash]; ok && current == st {
		delete(s.torrents, hash)
	}
	stErr := st.close()
	st.tor.Drop()
	return stErr
}

func (s *Session) RenameMetadata(ctx context.Context, oldName, newName string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !validMetadataName(oldName) || !validMetadataName(newName) || oldName == newName {
		return fmt.Errorf("session: metadata rename %q to %q: %w", oldName, newName, filesystem.ErrInvalidName)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("session: rename metadata: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureActiveLocked(); err != nil {
		return err
	}
	if _, ok := s.pendingWriters[newName]; ok {
		return fmt.Errorf("session: metadata %q: %w", newName, filesystem.ErrExists)
	}
	oldPath, newPath := s.metadataPath(oldName), s.metadataPath(newName)
	oldInfo, err := os.Lstat(oldPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("session: metadata %q: %w", oldName, filesystem.ErrNotFound)
		}
		return fmt.Errorf("session: stat metadata %q: %w", oldName, err)
	}
	if oldInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("session: metadata %q is a symlink: %w", oldName, filesystem.ErrPermission)
	}
	if !oldInfo.Mode().IsRegular() {
		return fmt.Errorf("session: metadata %q: %w", oldName, filesystem.ErrIsDir)
	}
	if _, err := os.Lstat(newPath); err == nil {
		return fmt.Errorf("session: metadata %q: %w", newName, filesystem.ErrExists)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session: stat metadata %q: %w", newName, err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return fmt.Errorf("session: rename metadata %q to %q: %w", oldName, newName, err)
	}
	if hash, ok := s.metadata[oldName]; ok {
		delete(s.metadata, oldName)
		s.metadata[newName] = hash
	}
	return nil
}

func (s *Session) EnsureMetadataDir() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureActiveLocked(); err != nil {
		return err
	}
	if err := os.Mkdir(s.metadataDir, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("session: metadata dir: %w", filesystem.ErrExists)
		}
		return fmt.Errorf("session: create metadata dir: %w", err)
	}
	return nil
}

func (s *Session) RemoveMetadataDir() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureActiveLocked(); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.metadataDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("session: metadata dir: %w", filesystem.ErrNotFound)
		}
		return fmt.Errorf("session: read metadata dir: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("session: remove metadata dir: %w", filesystem.ErrNotEmpty)
	}
	if err := os.Remove(s.metadataDir); err != nil {
		return fmt.Errorf("session: remove metadata dir: %w", err)
	}
	return nil
}
