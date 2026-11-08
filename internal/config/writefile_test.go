package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteBackFileReplacesTheFileAndKeepsABackup covers the contract of the
// atomic replacement: the new document is the file content, the previous
// content survives in the backup, and the permission of the original file is
// kept.
func TestWriteBackFileReplacesTheFileAndKeepsABackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "torrentfs.toml")
	previous := []byte("username = \"alice\"\n")
	if err := os.WriteFile(path, previous, 0o640); err != nil {
		t.Fatalf("write the configuration file: %v", err)
	}
	document := []byte("username = \"alice\"\n\n[log]\nlevel = \"debug\"\n")

	published, err := WriteBackFile(path, document)
	if err != nil {
		t.Fatalf("WriteBackFile: %v", err)
	}
	if !published {
		t.Fatal("WriteBackFile reported that the replacement was not published")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the configuration file: %v", err)
	}
	if string(got) != string(document) {
		t.Fatalf("the configuration file = %q, want %q", got, document)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the configuration file: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("the configuration file mode = %o, want 640", info.Mode().Perm())
	}

	backupPath := WriteBackBackupPath(path)
	backup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read the backup: %v", err)
	}
	if string(backup) != string(previous) {
		t.Fatalf("the backup = %q, want the previous content %q", backup, previous)
	}
	backupInfo, err := os.Stat(backupPath)
	if err != nil {
		t.Fatalf("stat the backup: %v", err)
	}
	if backupInfo.Mode().Perm() != 0o640 {
		t.Fatalf("the backup mode = %o, want 640", backupInfo.Mode().Perm())
	}
}

// TestWriteBackFileLeavesTheTargetUnchangedBeforeTheRename covers the failure
// contract: a failure before the rename commits leaves the file exactly as it
// was, and no temporary file survives.
func TestWriteBackFileLeavesTheTargetUnchangedBeforeTheRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "torrentfs.toml")
	previous := []byte("username = \"alice\"\n")
	if err := os.WriteFile(path, previous, 0o600); err != nil {
		t.Fatalf("write the configuration file: %v", err)
	}
	// The backup is renamed first; the replacement is the second rename.
	renames := 0
	restore := setWriteBackRenameHookForTest(func(temporary, target string) error {
		renames++
		if renames == 2 {
			return errors.New("rename refused")
		}
		return os.Rename(temporary, target)
	})
	defer restore()

	published, err := WriteBackFile(path, []byte("username = \"bob\"\n"))
	if published {
		t.Fatal("WriteBackFile reported a commit before the rename")
	}
	if !errors.Is(err, ErrWriteBackUnavailable) {
		t.Fatalf("WriteBackFile error = %v, want ErrWriteBackUnavailable", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read the configuration file: %v", readErr)
	}
	if string(got) != string(previous) {
		t.Fatalf("the configuration file = %q, want it unchanged at %q", got, previous)
	}
	if leftovers := temporaryWriteBackFiles(t, dir); len(leftovers) != 0 {
		t.Fatalf("temporary files survived the failed write: %v", leftovers)
	}
	backup, readErr := os.ReadFile(WriteBackBackupPath(path))
	if readErr != nil {
		t.Fatalf("read the backup: %v", readErr)
	}
	if string(backup) != string(previous) {
		t.Fatalf("the backup = %q, want the previous content %q", backup, previous)
	}
}

// TestWriteBackFileReportsACommittedWriteWhenTheDirectorySyncFails covers the
// contract that the rename is the commit point: the new content is already
// the file when only the directory sync fails, so the caller must adopt it
// and report that its persistence is unconfirmed.
func TestWriteBackFileReportsACommittedWriteWhenTheDirectorySyncFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "torrentfs.toml")
	if err := os.WriteFile(path, []byte("username = \"alice\"\n"), 0o600); err != nil {
		t.Fatalf("write the configuration file: %v", err)
	}
	// The backup is synced first; the replacement is the second sync.
	syncs := 0
	restore := setWriteBackSyncDirectoryHookForTest(func(string) error {
		syncs++
		if syncs == 2 {
			return errors.New("directory sync refused")
		}
		return nil
	})
	defer restore()

	document := []byte("username = \"bob\"\n")
	published, err := WriteBackFile(path, document)
	if !published {
		t.Fatal("WriteBackFile did not report the committed write")
	}
	if !errors.Is(err, ErrWriteBackDurabilityUnconfirmed) {
		t.Fatalf("WriteBackFile error = %v, want ErrWriteBackDurabilityUnconfirmed", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read the configuration file: %v", readErr)
	}
	if string(got) != string(document) {
		t.Fatalf("the configuration file = %q, want the published %q", got, document)
	}
}

// TestWriteBackFileRejectsAMissingConfigurationFile covers the read-only
// reason: a configuration file that does not exist cannot be replaced.
func TestWriteBackFileRejectsAMissingConfigurationFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "torrentfs.toml")
	published, err := WriteBackFile(path, []byte("username = \"alice\"\n"))
	if published {
		t.Fatal("WriteBackFile reported a commit for a missing file")
	}
	if !errors.Is(err, ErrWriteBackUnavailable) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("WriteBackFile error = %v, want ErrWriteBackUnavailable and os.ErrNotExist", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("WriteBackFile created the missing file: %v", statErr)
	}
}

// temporaryWriteBackFiles returns the temporary files the writer left in dir.
func temporaryWriteBackFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	var names []string
	for _, entry := range entries {
		if matched, _ := filepath.Match(".torrentfs-config-*.tmp", entry.Name()); matched {
			names = append(names, entry.Name())
		}
	}
	return names
}
