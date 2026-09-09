package filesystem

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestErrnoForWrappedErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want syscall.Errno
	}{
		{name: "exists", err: fmt.Errorf("outer: %w", ErrExists), want: syscall.EEXIST},
		{name: "not found", err: fmt.Errorf("outer: %w", ErrNotFound), want: syscall.ENOENT},
		{name: "is dir", err: fmt.Errorf("outer: %w", ErrIsDir), want: syscall.EISDIR},
		{name: "not dir", err: fmt.Errorf("outer: %w", ErrNotDir), want: syscall.ENOTDIR},
		{name: "not empty", err: fmt.Errorf("outer: %w", ErrNotEmpty), want: syscall.ENOTEMPTY},
		{name: "read only", err: fmt.Errorf("outer: %w", ErrReadOnly), want: syscall.EROFS},
		{name: "invalid", err: fmt.Errorf("outer: %w", ErrInvalidName), want: syscall.EINVAL},
		{name: "cross dir", err: fmt.Errorf("outer: %w", ErrCrossDir), want: syscall.EXDEV},
		{name: "path error", err: &os.PathError{Op: "open", Path: "x", Err: fs.ErrNotExist}, want: syscall.ENOENT},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := errnoFor(tc.err); got != tc.want {
				t.Fatalf("errnoFor(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestValidMetadataName(t *testing.T) {
	for _, name := range []string{"payload.x0.torrent", ".hidden.torrent", "a-b.torrent"} {
		if !validMetadataName(name) {
			t.Errorf("validMetadataName(%q) = false", name)
		}
	}
	for _, name := range []string{"", ".", "..", "payload", "../payload.torrent", "a/b.torrent", "a\\b.torrent", "payload.torrent.tmp"} {
		if validMetadataName(name) {
			t.Errorf("validMetadataName(%q) = true", name)
		}
	}
}

func TestRootEntriesReserveMetadata(t *testing.T) {
	entries := rootEntries([]TorrentView{{Name: metadataName, Hash: metainfo.Hash{1}}})
	if len(entries) != 1 || entries[0].Name == metadataName {
		t.Fatalf("rootEntries = %+v, metadata name was not disambiguated", entries)
	}
}

func TestReadOnlyTorrentDirectoryRejectsMutations(t *testing.T) {
	dir := &torrentDirNode{}
	if _, errno := dir.Mkdir(nil, "new", 0, &fuse.EntryOut{}); errno != syscall.EROFS {
		t.Fatalf("Mkdir errno = %v, want EROFS", errno)
	}
	if _, _, _, errno := dir.Create(nil, "new.torrent", 0, 0, &fuse.EntryOut{}); errno != syscall.EROFS {
		t.Fatalf("Create errno = %v, want EROFS", errno)
	}
	if errno := dir.Unlink(nil, "file"); errno != syscall.EROFS {
		t.Fatalf("Unlink errno = %v, want EROFS", errno)
	}
	if errno := dir.Rmdir(nil, "dir"); errno != syscall.EROFS {
		t.Fatalf("Rmdir errno = %v, want EROFS", errno)
	}
	if errno := dir.Rename(nil, "old", nil, "new", 0); errno != syscall.EROFS {
		t.Fatalf("Rename errno = %v, want EROFS", errno)
	}
}
