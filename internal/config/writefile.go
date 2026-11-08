package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrWriteBackUnavailable reports that the configuration file could not be
// replaced, so the file on disk is unchanged. A missing file, a path that is
// not a regular file, and a permission error are all this error: a caller
// reports them as a read-only configuration, not as a failed save.
var ErrWriteBackUnavailable = errors.New("config: write-back storage unavailable")

// ErrWriteBackDurabilityUnconfirmed reports that the new configuration file
// was published but syncing its directory failed. The rename already
// committed, so every reader and a restart observe the new content even
// though a crash may lose the directory entry.
var ErrWriteBackDurabilityUnconfirmed = errors.New("config: write-back durability unconfirmed")

// WriteBackBackupPath returns the path of the backup that a write-back keeps
// of the configuration file at path.
func WriteBackBackupPath(path string) string {
	return path + ".bak"
}

// WriteBackFile atomically replaces the configuration file at path with
// document.
//
// The previous content is copied to the backup path first. The new content is
// written to a temporary file in the same directory, synced, renamed over the
// target, and the directory is synced. The permission bits of the original
// file are kept.
//
// The returned published flag reports whether the rename committed. A failure
// before it leaves the target file exactly as it was and returns false. A
// failure of the directory sync after it returns true with
// ErrWriteBackDurabilityUnconfirmed: the new content is already visible to
// readers and to a restart, so the caller must adopt it instead of treating
// the save as failed.
func WriteBackFile(path string, document []byte) (published bool, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, fmt.Errorf("%w: inspect %q: %w", ErrWriteBackUnavailable, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("%w: %q is not a regular file", ErrWriteBackUnavailable, path)
	}
	previous, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("%w: read %q: %w", ErrWriteBackUnavailable, path, err)
	}
	mode := info.Mode().Perm()
	if _, err := writeConfigFileAtomic(WriteBackBackupPath(path), previous, mode); err != nil {
		return false, fmt.Errorf("%w: write the backup of %q: %w", ErrWriteBackUnavailable, path, err)
	}
	published, err = writeConfigFileAtomic(path, document, mode)
	if err != nil {
		if published {
			return true, fmt.Errorf("%w: %w", ErrWriteBackDurabilityUnconfirmed, err)
		}
		return false, fmt.Errorf("%w: %w", ErrWriteBackUnavailable, err)
	}
	return true, nil
}

// writeConfigFileAtomic writes data through a temporary file in the directory
// of path, an fsync, and an atomic rename, and reports whether the rename
// committed.
func writeConfigFileAtomic(path string, data []byte, mode os.FileMode) (bool, error) {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".torrentfs-config-*.tmp")
	if err != nil {
		return false, err
	}
	temporary := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(temporary)
	}
	if _, err := file.Write(data); err != nil {
		cleanup()
		return false, err
	}
	if err := file.Chmod(mode); err != nil {
		cleanup()
		return false, err
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return false, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return false, err
	}
	if err := renameWriteBackTemporary(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return false, err
	}
	if err := syncWriteBackDirectory(dir); err != nil {
		return true, fmt.Errorf("sync the directory of %q: %w", path, err)
	}
	return true, nil
}

// renameWriteBackTemporary renames a written temporary file over the target.
func renameWriteBackTemporary(temporary, path string) error {
	if writeBackRenameHook != nil {
		return writeBackRenameHook(temporary, path)
	}
	return os.Rename(temporary, path)
}

// syncWriteBackDirectory syncs a directory so a rename it holds survives a
// crash.
func syncWriteBackDirectory(dir string) error {
	if writeBackSyncDirectoryHook != nil {
		return writeBackSyncDirectoryHook(dir)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

// The hooks let a test reach the failure paths of the atomic replacement.
// Production leaves them nil.
var (
	writeBackRenameHook        func(temporary, path string) error
	writeBackSyncDirectoryHook func(dir string) error
)
