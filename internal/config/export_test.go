package config

// setWriteBackRenameHookForTest makes the rename of the atomic replacement
// fail, so a test can exercise the failure path before the commit. It returns
// a restore function. Test-only.
func setWriteBackRenameHookForTest(fn func(temporary, path string) error) func() {
	previous := writeBackRenameHook
	writeBackRenameHook = fn
	return func() { writeBackRenameHook = previous }
}

// setWriteBackSyncDirectoryHookForTest makes the directory sync of the atomic
// replacement fail, so a test can exercise the published-but-unconfirmed
// path. It returns a restore function. Test-only.
func setWriteBackSyncDirectoryHookForTest(fn func(string) error) func() {
	previous := writeBackSyncDirectoryHook
	writeBackSyncDirectoryHook = fn
	return func() { writeBackSyncDirectoryHook = previous }
}
