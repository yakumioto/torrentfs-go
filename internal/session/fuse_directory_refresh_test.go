package session_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"golang.org/x/sys/unix"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

func testFuseVisitedDirectoriesRefreshSubtitles(t *testing.T, strm bool) {
	t.Helper()
	requireFuse(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	work := t.TempDir()
	cfg := testConfig()
	cfg.STRM.BaseURL = "http://example.test"
	sess := newLoopbackSession(t, cfg, filepath.Join(work, "torrents"))
	torrentPath, hash, _ := buildMultiFileTorrent(t, work, "Collection", map[string][]byte{
		"top.mkv":                []byte("unseeded top video"),
		"season/deep/Movie.mkv":  []byte("unseeded deep video"),
		"season/deep/z-last.txt": []byte("unseeded notes"),
	})
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatal(err)
	}
	var backend filesystem.Backend = sess
	if strm {
		backend = filesystem.NewSTRMBackend(sess)
	}
	mnt := filepath.Join(work, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	server, err := filesystem.Mount(mnt, backend, &fs.Options{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unmountServer(t, server, mnt) })
	root := filepath.Join(mnt, "Collection")
	deep := filepath.Join(root, "season", "deep")
	video := "Movie.mkv"
	if strm {
		video = "Movie.strm"
	}
	// Visit both levels before any upload, even without retaining an fd to the
	// top directory. go-fuse keeps those inodes after readdir has returned.
	for _, dir := range []string{root, deep} {
		if _, err := os.ReadDir(dir); err != nil {
			t.Fatalf("visit directory before upload: %v", err)
		}
	}
	held, err := os.Open(deep)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	before, err := held.Stat()
	if err != nil {
		t.Fatal(err)
	}
	first, err := held.Readdirnames(1)
	if err != nil || !reflect.DeepEqual(first, []string{video}) {
		t.Fatalf("old directory first page = %v/%v, want %s", first, err, video)
	}
	upload := func(videoPath, filename, content string) {
		t.Helper()
		if _, err := sess.UploadSubtitle(ctx, hash.HexString(), videoPath, filename, strings.NewReader(content), 1<<20); err != nil {
			t.Fatalf("UploadSubtitle %s: %v", filename, err)
		}
	}
	upload("top.mkv", "top.srt", "top subtitle\n")
	const original = "old subtitle\n"
	upload("season/deep/Movie.mkv", "Movie.srt", original)
	fuseSTRMReadFile(t, filepath.Join(root, "top.srt"), []byte("top subtitle\n"))
	path := filepath.Join(deep, "Movie.srt")
	fuseSTRMReadFile(t, path, []byte(original))
	checkNewListing := func() {
		t.Helper()
		entries, err := os.ReadDir(deep)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		want := []string{video, "Movie.srt", "z-last.txt"}
		slices.Sort(want)
		if !reflect.DeepEqual(names, want) {
			t.Fatalf("new listing = %v, want %v", names, want)
		}
	}
	checkNewListing()
	if rest, err := held.Readdirnames(-1); err != nil || !reflect.DeepEqual(rest, []string{"z-last.txt"}) {
		t.Fatalf("old directory remainder = %v/%v, want unchanged list and offset", rest, err)
	}
	after, err := os.Stat(deep)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("visited directory inode changed: %v", err)
	}
	// Resolve through the retained directory fd, not just a new root traversal.
	openAt := func() *os.File {
		t.Helper()
		fd, err := unix.Openat(int(held.Fd()), "Movie.srt", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatalf("openat old directory: %v", err)
		}
		file := os.NewFile(uintptr(fd), "Movie.srt")
		t.Cleanup(func() { _ = file.Close() })
		return file
	}
	old := openAt()
	for _, content := range []string{"new subtitle\n", strings.Repeat("long replacement subtitle\n", 128)} {
		upload("season/deep/Movie.mkv", "Movie.srt", content)
		checkNewListing()
		fuseSTRMReadFile(t, path, []byte(content))
		current := openAt()
		if got, err := io.ReadAll(current); err != nil || string(got) != content {
			t.Fatalf("new openat read = %d bytes/%v, want %d identical bytes", len(got), err, len(content))
		}
		if err := current.Close(); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, len(original)+16)
		n, err := old.ReadAt(buf, 0)
		if !errors.Is(err, io.EOF) || string(buf[:n]) != original {
			t.Fatalf("old subtitle handle = %q/%v, want old descriptor snapshot", buf[:n], err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	op, err := sess.DeleteTorrent(ctx, hash.HexString())
	if err != nil {
		t.Fatal(err)
	}
	if final := waitOperation(t, sess, op.ID); final.State != session.StateDeleted {
		t.Fatalf("delete operation = %+v", final)
	}
	if _, err := held.Stat(); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("retained directory getattr after delete = %v, want ENOENT", err)
	}
	if fd, err := unix.Openat(int(held.Fd()), "Movie.srt", unix.O_RDONLY|unix.O_CLOEXEC, 0); !errors.Is(err, syscall.ENOENT) {
		if err == nil {
			_ = unix.Close(fd)
		}
		t.Fatalf("retained directory lookup after delete = %v, want ENOENT", err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	stats := sess.RuntimeStats()
	if stats.DownloadedBytes != 0 || stats.UploadedBytes != 0 || stats.CacheUsedBytes != 0 {
		t.Fatalf("subtitle refresh read unseeded payload: %+v", stats)
	}
}
