package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// subtitleStore is the one trusted handle for the managed subtitle tree. It is
// opened once while the session starts and every later operation is resolved
// component by component from this handle with O_NOFOLLOW, so neither a swap of
// the store's own path nor a symlink planted inside it can redirect a subtitle
// read or write:
//
//   - the handle keeps referring to the directory it was opened on, even if that
//     directory is later moved away from the configured path;
//   - a component that is a symlink is refused by O_NOFOLLOW|O_DIRECTORY, so an
//     in-store link cannot make one torrent's operation land in another
//     torrent's tree.
//
// Paths passed to these methods are always relative to the store and are refused
// unless every component is a plain name.
type subtitleStore struct {
	path string
	dir  *os.File
}

const subtitleStoreDirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

func openSubtitleStore(path string) (*subtitleStore, error) {
	fd, err := unix.Open(path, subtitleStoreDirFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("session: open subtitle store %q: %w", path, err)
	}
	return &subtitleStore{path: path, dir: os.NewFile(uintptr(fd), path)}, nil
}

func (s *subtitleStore) Close() error {
	if s == nil || s.dir == nil {
		return nil
	}
	return s.dir.Close()
}

// verify reports whether the configured path still names the directory this
// handle was opened on, so a replaced store is refused instead of silently
// receiving writes into a directory nobody expects to see them in. It is not
// what confines the operations: those use the handle either way.
func (s *subtitleStore) verify() error {
	if s == nil || s.dir == nil {
		return errors.New("session: subtitle store is not open")
	}
	var opened unix.Stat_t
	if err := unix.Fstat(int(s.dir.Fd()), &opened); err != nil {
		return fmt.Errorf("session: inspect subtitle store: %w", err)
	}
	var current unix.Stat_t
	if err := unix.Lstat(s.path, &current); err != nil {
		return fmt.Errorf("session: subtitle store %q was replaced: %w", s.path, err)
	}
	if opened.Dev != current.Dev || opened.Ino != current.Ino {
		return fmt.Errorf("session: subtitle store %q was replaced", s.path)
	}
	return nil
}

// subtitleStoreComponents splits a store-relative path and refuses anything that
// is not a plain component chain.
func subtitleStoreComponents(rel string) ([]string, error) {
	if rel == "" || rel == "." {
		return nil, nil
	}
	if filepath.IsAbs(rel) {
		return nil, fmt.Errorf("session: subtitle path %q must stay inside the store", rel)
	}
	components := strings.Split(filepath.ToSlash(rel), "/")
	for _, component := range components {
		if component == "" || component == "." || component == ".." || strings.ContainsAny(component, "\\\x00") {
			return nil, fmt.Errorf("session: invalid subtitle path %q", rel)
		}
	}
	return components, nil
}

// splitStorePath separates a store-relative path into its directory components
// and its final name.
func splitStorePath(rel string) ([]string, string, error) {
	components, err := subtitleStoreComponents(rel)
	if err != nil {
		return nil, "", err
	}
	if len(components) == 0 {
		return nil, "", fmt.Errorf("session: subtitle path %q has no name", rel)
	}
	return components[:len(components)-1], components[len(components)-1], nil
}

// openDirChain opens a directory inside the store one component at a time. A
// component that is not a real directory is refused by the open flags, so a
// symlink is never traversed even when it points inside the store.
func (s *subtitleStore) openDirChain(components []string) (*os.File, error) {
	dup, err := unix.Openat(int(s.dir.Fd()), ".", subtitleStoreDirFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("session: open subtitle store: %w", err)
	}
	dir := os.NewFile(uintptr(dup), s.path)
	for _, component := range components {
		name := filepath.Join(dir.Name(), component)
		next, err := unix.Openat(int(dir.Fd()), component, subtitleStoreDirFlags, 0)
		_ = dir.Close()
		if err != nil {
			return nil, fmt.Errorf("session: open subtitle path %q: %w", name, err)
		}
		dir = os.NewFile(uintptr(next), name)
	}
	return dir, nil
}

func (s *subtitleStore) openDir(rel string) (*os.File, error) {
	components, err := subtitleStoreComponents(rel)
	if err != nil {
		return nil, err
	}
	return s.openDirChain(components)
}

// mkdirAll creates a directory chain inside the store, re-opening every created
// level with the no-follow flags so a component that raced a symlink into place
// is refused.
func (s *subtitleStore) mkdirAll(rel string, mode os.FileMode) error {
	components, err := subtitleStoreComponents(rel)
	if err != nil || len(components) == 0 {
		return err
	}
	dir, err := s.openDirChain(nil)
	if err != nil {
		return err
	}
	for _, component := range components {
		name := filepath.Join(dir.Name(), component)
		next, err := unix.Openat(int(dir.Fd()), component, subtitleStoreDirFlags, 0)
		if errors.Is(err, unix.ENOENT) {
			if mkErr := unix.Mkdirat(int(dir.Fd()), component, uint32(mode.Perm())); mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
				_ = dir.Close()
				return fmt.Errorf("session: create subtitle path %q: %w", name, mkErr)
			}
			next, err = unix.Openat(int(dir.Fd()), component, subtitleStoreDirFlags, 0)
		}
		_ = dir.Close()
		if err != nil {
			return fmt.Errorf("session: open subtitle path %q: %w", name, err)
		}
		dir = os.NewFile(uintptr(next), name)
	}
	return dir.Close()
}

// createExclusive creates one file inside the store. The final component is
// opened with O_EXCL|O_NOFOLLOW, so an existing entry or a symlink is refused
// rather than replaced or followed.
func (s *subtitleStore) createExclusive(rel string, mode os.FileMode) (*os.File, error) {
	dir, name, err := s.openParent(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return nil, fmt.Errorf("session: create subtitle file %q: %w", rel, err)
	}
	return os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name)), nil
}

// openRead opens one file inside the store for reading, without following a
// symlink at the final component.
func (s *subtitleStore) openRead(rel string) (*os.File, error) {
	dir, name, err := s.openParent(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("session: open subtitle file %q: %w", rel, err)
	}
	return os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name)), nil
}

// rename moves one file inside the store. renameat operates on the entries
// themselves, so neither side is followed and a symlink standing in for the
// destination is replaced rather than written through.
func (s *subtitleStore) rename(oldRel, newRel string) error {
	oldDir, oldName, err := s.openParent(oldRel)
	if err != nil {
		return err
	}
	defer func() { _ = oldDir.Close() }()
	newDir, newName, err := s.openParent(newRel)
	if err != nil {
		return err
	}
	defer func() { _ = newDir.Close() }()
	if err := unix.Renameat(int(oldDir.Fd()), oldName, int(newDir.Fd()), newName); err != nil {
		return fmt.Errorf("session: publish subtitle %q: %w", newRel, err)
	}
	return nil
}

// remove deletes one entry inside the store, treating a missing entry as done.
// A symlink is unlinked rather than followed.
func (s *subtitleStore) remove(rel string) error {
	dir, name, err := s.openParent(rel)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	if err := unix.Unlinkat(int(dir.Fd()), name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("session: remove subtitle entry %q: %w", rel, err)
	}
	return nil
}

// lstat reports one entry's metadata without following a symlink.
func (s *subtitleStore) lstat(rel string) (unix.Stat_t, error) {
	dir, name, err := s.openParent(rel)
	if err != nil {
		return unix.Stat_t{}, err
	}
	defer func() { _ = dir.Close() }()
	var stat unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return unix.Stat_t{}, &os.PathError{Op: "fstatat", Path: filepath.Join(dir.Name(), name), Err: err}
	}
	return stat, nil
}

// readDir lists a directory inside the store, refusing to traverse a symlink.
func (s *subtitleStore) readDir(rel string) ([]os.DirEntry, error) {
	dir, err := s.openDir(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("session: list subtitle path %q: %w", rel, err)
	}
	return entries, nil
}

// syncDir fsyncs a directory inside the store.
func (s *subtitleStore) syncDir(rel string) error {
	dir, err := s.openDir(rel)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

// openParent opens the directory that holds the final component of rel.
func (s *subtitleStore) openParent(rel string) (*os.File, string, error) {
	dirComponents, name, err := splitStorePath(rel)
	if err != nil {
		return nil, "", err
	}
	dir, err := s.openDirChain(dirComponents)
	if err != nil {
		return nil, "", err
	}
	return dir, name, nil
}

const (
	subtitleStoreFileType    = unix.S_IFMT
	subtitleStoreRegular     = unix.S_IFREG
	subtitleStoreDirectory   = unix.S_IFDIR
	subtitleStoreStagingGlob = ".torrentfs-subtitle-"
)

// walkFiles lists every regular file under rel, refusing symlinks and special
// entries. Staging files abandoned by a crash carry the internal prefix and are
// removed instead of being reported. Nothing is followed, so a planted link is
// an error the caller surfaces rather than a way into another tree.
func (s *subtitleStore) walkFiles(rel string, out *[]string) error {
	entries, err := s.readDir(rel)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := filepath.Join(rel, entry.Name())
		stat, err := s.lstat(child)
		if err != nil {
			return err
		}
		if stat.Mode&subtitleStoreFileType == subtitleStoreDirectory {
			if err := s.walkFiles(child, out); err != nil {
				return err
			}
			continue
		}
		if stat.Mode&subtitleStoreFileType != subtitleStoreRegular {
			return fmt.Errorf("session: subtitle path %q is not a regular file", child)
		}
		if strings.HasPrefix(entry.Name(), subtitleStoreStagingGlob) && strings.HasSuffix(entry.Name(), ".tmp") {
			if err := s.remove(child); err != nil {
				return err
			}
			continue
		}
		*out = append(*out, child)
	}
	return nil
}

// validateTree reports whether a whole subtree holds only real directories and
// regular files, so a caller that is about to delete it can refuse host-planted
// links and devices first.
func (s *subtitleStore) validateTree(rel string) error {
	entries, err := s.readDir(rel)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := filepath.Join(rel, entry.Name())
		stat, err := s.lstat(child)
		if err != nil {
			return err
		}
		switch stat.Mode & subtitleStoreFileType {
		case subtitleStoreDirectory:
			if err := s.validateTree(child); err != nil {
				return err
			}
		case subtitleStoreRegular:
		default:
			return fmt.Errorf("session: subtitle path %q is not a regular file", child)
		}
	}
	return nil
}

// removeTree deletes one subtree inside the store. Descent happens only through
// directory handles opened with the no-follow flags, and every entry is removed
// with unlinkat, so a link is unlinked rather than traversed.
func (s *subtitleStore) removeTree(rel string) error {
	dirComponents, name, err := splitStorePath(rel)
	if err != nil {
		return err
	}
	parent, err := s.openDirChain(dirComponents)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = parent.Close() }()
	return removeStoreEntry(int(parent.Fd()), name)
}

func removeStoreEntry(parentFd int, name string) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(parentFd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("session: inspect subtitle entry %q: %w", name, err)
	}
	if stat.Mode&subtitleStoreFileType != subtitleStoreDirectory {
		if err := unix.Unlinkat(parentFd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("session: remove subtitle entry %q: %w", name, err)
		}
		return nil
	}
	fd, err := unix.Openat(parentFd, name, subtitleStoreDirFlags, 0)
	if err != nil {
		return fmt.Errorf("session: open subtitle directory %q: %w", name, err)
	}
	dir := os.NewFile(uintptr(fd), name)
	entries, err := dir.ReadDir(-1)
	if err != nil {
		_ = dir.Close()
		return fmt.Errorf("session: list subtitle directory %q: %w", name, err)
	}
	for _, entry := range entries {
		if err := removeStoreEntry(int(dir.Fd()), entry.Name()); err != nil {
			_ = dir.Close()
			return err
		}
	}
	_ = dir.Close()
	if err := unix.Unlinkat(parentFd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("session: remove subtitle directory %q: %w", name, err)
	}
	return nil
}
