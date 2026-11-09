package config

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// WriteBackAvailable reports whether the configuration file at path exists and
// can be replaced by WriteBackFile.
//
// exists is false when there is no file to replace. writable then reports
// whether the process can atomically replace it: the target is a regular file,
// the file and its directory carry a write bit, and the access check agrees.
// A symlink is not writable, matching the write-back, which replaces the target
// path instead of following the link.
func WriteBackAvailable(path string) (exists, writable bool, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("config: inspect the configuration file %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return true, false, nil
	}
	if info.Mode().Perm()&0o222 == 0 || unix.Access(path, unix.W_OK) != nil {
		return true, false, nil
	}
	directoryPath := filepath.Dir(path)
	directory, err := os.Stat(directoryPath)
	if err != nil {
		return true, false, fmt.Errorf("config: inspect the configuration directory %q: %w", directoryPath, err)
	}
	if !directory.IsDir() || directory.Mode().Perm()&0o222 == 0 || unix.Access(directoryPath, unix.W_OK) != nil {
		return true, false, nil
	}
	return true, true, nil
}
