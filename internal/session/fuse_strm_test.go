package session_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"

	"github.com/yakumioto/torrentfs-go/internal/api"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

type fuseSTRMPayload struct {
	hash      metainfo.Hash
	original  string
	mounted   string
	projected string
	data      []byte
	generated bool
	link      string
}

func fuseSTRMContent(length, salt int) []byte {
	data := make([]byte, length)
	for i := range data {
		data[i] = byte(i*31 + i/251 + salt)
	}
	return data
}

func fuseSTRMAssertTree(t *testing.T, mountpoint string, want []string) {
	t.Helper()
	var got []string
	err := filepath.WalkDir(mountpoint, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o444)
		if entry.IsDir() {
			mode = 0o555
		} else if !info.Mode().IsRegular() {
			return fmt.Errorf("%s mode = %v, want a regular file", path, info.Mode())
		}
		if info.Mode().Perm() != mode {
			return fmt.Errorf("%s permissions = %o, want %o", path, info.Mode().Perm(), mode)
		}
		if path == mountpoint {
			return nil
		}
		rel, err := filepath.Rel(mountpoint, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			rel += "/"
		}
		got = append(got, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk mounted tree: %v", err)
	}
	want = slices.Clone(want)
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mounted tree = %v, want %v", got, want)
	}
}

func fuseSTRMReadFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s returned %d bytes, want %d identical bytes", path, len(got), len(want))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Size() != int64(len(got)) {
		t.Fatalf("%s size = %d, read %d bytes", path, info.Size(), len(got))
	}
}

// The configured URL stays unchanged for the entire Session lifetime. Only the
// request destination is replaced, so the actual transport reaches httptest
// without mutating a running server's Handler or the session's configuration.
func fuseSTRMRequest(t *testing.T, ctx context.Context, client *http.Client, destination, signedURL string, start, length int64) (*http.Response, []byte) {
	t.Helper()
	location, err := url.Parse(signedURL)
	if err != nil {
		t.Fatal("parse signed stream URL")
	}
	if location.Scheme != "http" || location.Host != "example.test" || !strings.HasPrefix(location.Path, "/stream/") {
		t.Fatal("signed stream URL did not use the configured base URL")
	}
	target, err := url.Parse(destination)
	if err != nil {
		t.Fatal("parse loopback HTTP destination")
	}
	location.Scheme, location.Host = target.Scheme, target.Host
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, location.String(), nil)
	if err != nil {
		t.Fatal("create stream request")
	}
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+length-1))
	response, err := client.Do(request)
	if err != nil {
		// Client errors can embed the complete file-scoped capability URL.
		t.Fatal("stream HTTP request failed")
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, length+1))
	if err != nil {
		t.Fatalf("read stream response: %v", err)
	}
	return response, body
}

// TestFuseStrmDualMountSignedRangeAndDeletion exercises the real kernel view,
// signed HTTP transport and the shared Session without fetching any payload
// until after an unseeded media-library scan has finished.
func TestFuseStrmDualMountSignedRangeAndDeletion(t *testing.T) {
	requireFuse(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	work := t.TempDir()
	cfg := testConfig()
	cfg.STRM.BaseURL = "http://example.test"
	cfg.Cache.CapacityBytes = 16 << 20
	cfg.Mount.ReadTimeout = config.Duration(2 * time.Second)

	const (
		soloName       = "独立电影 %#?.mkv"
		soloSTRM       = "独立电影 %#?.strm"
		soloSubtitle   = "独立电影 %#?.vtt"
		category       = "剧集"
		multiName      = "合集"
		multiPrefix    = category + "/" + multiName + "/"
		nestedVideo    = "Season 1/第01集.mp4"
		nestedSTRM     = "Season 1/第01集.strm"
		nestedSubtitle = "Season 1/第01集.ass"
		discPath       = "Discs/原盘.ISO"
		discSTRM       = "Discs/原盘.iso.strm"
	)
	soloData := fuseSTRMContent(2*testPieceLength+4096, 3)
	soloPath, soloHash := buildSingleFileTorrent(t, work, soloName, soloData)
	files := map[string][]byte{
		nestedVideo:         fuseSTRMContent(testPieceLength+16384, 7),
		discPath:            fuseSTRMContent(96*1024+17, 11),
		"notes.txt":         []byte("original non-video payload\n"),
		"existing.strm":     []byte("http://preexisting.invalid/original\n"),
		"Season 1/第01集.nfo": []byte("original episode metadata\n"),
		"Season 1/第01集.srt": []byte("1\n00:00:00,000 --> 00:00:01,000\npayload subtitle\n"),
	}
	multiPath, multiHash, multiData := buildMultiFileTorrent(t, work, multiName, files)
	sess := newLoopbackSession(t, cfg, filepath.Join(work, "torrents"))
	for _, source := range []string{soloPath, multiPath} {
		if _, err := sess.AddTorrentAndPersist(ctx, session.Source{MetainfoPath: source}); err != nil {
			t.Fatalf("AddTorrentAndPersist: %v", err)
		}
	}
	for _, name := range []string{category, "empty"} {
		if _, err := sess.CreateCategory(ctx, name); err != nil {
			t.Fatalf("CreateCategory: %v", err)
		}
	}
	if _, err := sess.SetTorrentCategory(ctx, multiHash.HexString(), category); err != nil {
		t.Fatalf("SetTorrentCategory: %v", err)
	}
	sidecars := map[string][]byte{
		soloSubtitle:                 []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nmanaged solo subtitle\n"),
		multiPrefix + nestedSubtitle: []byte("[Script Info]\nTitle: managed episode subtitle\n"),
	}
	for _, upload := range []struct {
		hash  metainfo.Hash
		video string
		name  string
		rel   string
	}{
		{soloHash, soloName, soloSubtitle, soloSubtitle},
		{multiHash, nestedVideo, "第01集.ass", multiPrefix + nestedSubtitle},
	} {
		if _, err := sess.UploadSubtitle(ctx, upload.hash.HexString(), upload.video, upload.name, bytes.NewReader(sidecars[upload.rel]), 1<<20); err != nil {
			t.Fatalf("UploadSubtitle: %v", err)
		}
	}

	originalMount := filepath.Join(work, "original")
	strmMount := filepath.Join(work, "strm")
	for _, mount := range []struct {
		path    string
		backend filesystem.Backend
	}{
		{originalMount, sess},
		{strmMount, filesystem.NewSTRMBackend(sess)},
	} {
		if err := os.Mkdir(mount.path, 0o755); err != nil {
			t.Fatalf("make mountpoint: %v", err)
		}
		server, err := filesystem.Mount(mount.path, mount.backend, &fs.Options{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())})
		if err != nil {
			t.Fatalf("Mount: %v", err)
		}
		t.Cleanup(func() { unmountServer(t, server, mount.path) })
	}

	service, err := api.New(cfg, sess)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	httpServer := httptest.NewServer(service.Handler())
	client := httpServer.Client()
	client.Timeout = 5 * time.Second
	t.Cleanup(func() {
		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := service.Shutdown(shutdownCtx); err != nil {
			t.Errorf("API shutdown: %v", err)
		}
		client.CloseIdleConnections()
		httpServer.CloseClientConnections()
		done := make(chan struct{})
		go func() {
			httpServer.Close()
			close(done)
		}()
		select {
		case <-done:
		case <-shutdownCtx.Done():
			t.Error("HTTP test server did not close before its deadline")
		}
	})

	payloads := []fuseSTRMPayload{{soloHash, soloName, soloName, soloSTRM, soloData, true, ""}}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		projected, generated := path, false
		switch path {
		case nestedVideo:
			projected, generated = nestedSTRM, true
		case discPath:
			projected, generated = discSTRM, true
		}
		payloads = append(payloads, fuseSTRMPayload{multiHash, path, multiPrefix + path, multiPrefix + projected, files[path], generated, ""})
	}
	before := sess.RuntimeStats()
	if before.CacheUsedBytes != 0 || before.DownloadedBytes != 0 || before.UploadedBytes != 0 {
		t.Fatalf("unseeded fixture already used payload: %+v", before)
	}
	originalTree := []string{
		"empty/", category + "/", multiPrefix, multiPrefix + "Discs/", multiPrefix + "Season 1/",
		soloSubtitle, multiPrefix + nestedSubtitle,
	}
	projectedTree := slices.Clone(originalTree)
	for _, payload := range payloads {
		originalTree = append(originalTree, payload.mounted)
		projectedTree = append(projectedTree, payload.projected)
	}
	fuseSTRMAssertTree(t, originalMount, originalTree)
	fuseSTRMAssertTree(t, strmMount, projectedTree)
	for i := range payloads {
		payload := &payloads[i]
		info, err := os.Stat(filepath.Join(originalMount, filepath.FromSlash(payload.mounted)))
		if err != nil || info.Size() != int64(len(payload.data)) {
			t.Fatalf("original payload stat for %s = %v/%v", payload.mounted, info, err)
		}
		if !payload.generated {
			continue
		}
		payload.link, err = sess.StreamURL(payload.hash, payload.original)
		if err != nil {
			t.Fatalf("StreamURL: %v", err)
		}
		fuseSTRMReadFile(t, filepath.Join(strmMount, filepath.FromSlash(payload.projected)), []byte(payload.link+"\n"))
	}
	for path, content := range sidecars {
		for _, mount := range []string{originalMount, strmMount} {
			fuseSTRMReadFile(t, filepath.Join(mount, filepath.FromSlash(path)), content)
		}
	}
	for _, mount := range []string{originalMount, strmMount} {
		if err := os.WriteFile(filepath.Join(mount, "new.txt"), []byte("forbidden"), 0o644); !errors.Is(err, syscall.EROFS) {
			t.Fatalf("create through mount = %v, want EROFS", err)
		}
		if err := os.WriteFile(filepath.Join(mount, soloSubtitle), []byte("forbidden"), 0o644); !errors.Is(err, syscall.EROFS) {
			t.Fatalf("write managed subtitle through mount = %v, want EROFS", err)
		}
	}
	if err := os.WriteFile(filepath.Join(strmMount, soloSTRM), []byte("forbidden"), 0o644); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("write generated STRM = %v, want EROFS", err)
	}
	if err := os.Remove(filepath.Join(strmMount, soloSTRM)); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("unlink generated STRM = %v, want EROFS", err)
	}
	after := sess.RuntimeStats()
	if after.CacheUsedBytes != before.CacheUsedBytes || after.DownloadedBytes != before.DownloadedBytes || after.UploadedBytes != before.UploadedBytes {
		t.Fatalf("STRM listing/stat/read fetched payload: before=%+v after=%+v", before, after)
	}
	for _, hash := range []metainfo.Hash{soloHash, multiHash} {
		torrent, ok := sess.Torrent(hash)
		if !ok || torrent.CachedBytes() != 0 {
			t.Fatalf("unseeded scan populated cache for %s", hash)
		}
	}

	// Only now make real video and ISO bytes available. Both the original FUSE
	// mount and HTTP ranges must read exactly these same immutable payloads.
	for _, seed := range []struct {
		hash metainfo.Hash
		data []byte
	}{{soloHash, soloData}, {multiHash, multiData}} {
		seedPieces(t, sess, seed.hash, seed.data)
		torrent, ok := sess.Torrent(seed.hash)
		if !ok {
			t.Fatal("seeded torrent is not registered")
		}
		waitCached(t, ctx, torrent)
	}
	for _, payload := range payloads {
		fuseSTRMReadFile(t, filepath.Join(originalMount, filepath.FromSlash(payload.mounted)), payload.data)
		if !payload.generated {
			fuseSTRMReadFile(t, filepath.Join(strmMount, filepath.FromSlash(payload.projected)), payload.data)
			continue
		}
		start, length := int64(123), int64(4096)
		if payload.hash == soloHash {
			start = testPieceLength - 113
		}
		response, body := fuseSTRMRequest(t, ctx, client, httpServer.URL, payload.link, start, length)
		wantRange := fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, len(payload.data))
		if response.StatusCode != http.StatusPartialContent || response.ContentLength != length || response.Header.Get("Content-Range") != wantRange || !bytes.Equal(body, payload.data[start:start+length]) {
			t.Fatalf("stream range for %s = status %d, length %d, range %q, %d bytes; want exact source slice", payload.original, response.StatusCode, response.ContentLength, response.Header.Get("Content-Range"), len(body))
		}
	}

	for _, hash := range []metainfo.Hash{soloHash, multiHash} {
		op, err := sess.DeleteTorrent(ctx, hash.HexString())
		if err != nil {
			t.Fatalf("DeleteTorrent: %v", err)
		}
		if final := waitOperation(t, sess, op.ID); final.State != session.StateDeleted {
			t.Fatalf("delete operation = %+v, want deleted", final)
		}
	}
	if err := waitFor(ctx, func() bool {
		for _, payload := range payloads {
			for mount, rel := range map[string]string{originalMount: payload.mounted, strmMount: payload.projected} {
				if _, err := os.Stat(filepath.Join(mount, filepath.FromSlash(rel))); !errors.Is(err, os.ErrNotExist) {
					return false
				}
			}
		}
		return true
	}); err != nil {
		t.Fatalf("both mounts did not remove deleted torrents: %v", err)
	}
	for _, mount := range []string{originalMount, strmMount} {
		fuseSTRMAssertTree(t, mount, []string{"empty/", category + "/"})
	}
	for _, payload := range payloads {
		if !payload.generated {
			continue
		}
		if _, err := sess.OpenStream(streamLocationFor(t, payload.link)); !errors.Is(err, filesystem.ErrNotFound) {
			t.Fatalf("old signed capability after delete = %v, want ErrNotFound", err)
		}
		response, _ := fuseSTRMRequest(t, ctx, client, httpServer.URL, payload.link, 0, 64)
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("old signed HTTP link after delete = %d, want 404", response.StatusCode)
		}
	}
	t.Log("verified real dual FUSE mounts, metadata-only STRM reads, signed HTTP ranges and deletion")
}

type fuseSTRMDeleteResult struct {
	operation *session.Operation
	err       error
}

type fuseSTRMDeleteOnURL struct {
	*session.Session
	ctx     context.Context
	once    sync.Once
	deleted chan fuseSTRMDeleteResult
}

func (b *fuseSTRMDeleteOnURL) StreamURL(hash metainfo.Hash, path string) (string, error) {
	b.once.Do(func() {
		// The view snapshot already contains the payload. Mark it deleting before
		// signing, but do not wait for the background cleanup inside the listing.
		op, err := b.Session.DeleteTorrent(b.ctx, hash.HexString())
		b.deleted <- fuseSTRMDeleteResult{operation: op, err: err}
	})
	return b.Session.StreamURL(hash, path)
}

// The multi-file case retains the actual kernel directory handle whose Files
// snapshot contains a failed URL's zero size. Readding the same infohash must
// recover through that old directory and the stable inode bridge without ever
// treating the newly available STRM text as a successful zero-byte file.
func TestFuseStrmRecoversAfterURLRaceDeletesAndReaddsSameHash(t *testing.T) {
	requireFuse(t)
	for _, single := range []bool{true, false} {
		t.Run(fmt.Sprintf("single-file-%v", single), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			work := t.TempDir()
			cfg := testConfig()
			cfg.STRM.BaseURL = "http://example.test"
			cfg.Mount.ReadTimeout = config.Duration(time.Second)
			sess := newLoopbackSession(t, cfg, filepath.Join(work, "torrents"))
			var torrentPath string
			var hash metainfo.Hash
			if single {
				torrentPath, hash = buildSingleFileTorrent(t, work, "Movie.mkv", []byte("unseeded video payload"))
			} else {
				torrentPath, hash, _ = buildMultiFileTorrent(t, work, "Collection", map[string][]byte{
					"Movie.mkv": []byte("unseeded video payload"),
					"notes.txt": []byte("unseeded non-video payload"),
				})
			}
			if _, err := sess.AddTorrentAndPersist(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
				t.Fatalf("AddTorrentAndPersist: %v", err)
			}
			backend := &fuseSTRMDeleteOnURL{Session: sess, ctx: ctx, deleted: make(chan fuseSTRMDeleteResult, 1)}
			mountpoint := filepath.Join(work, "strm")
			if err := os.Mkdir(mountpoint, 0o755); err != nil {
				t.Fatalf("make mountpoint: %v", err)
			}
			server, err := filesystem.Mount(mountpoint, filesystem.NewSTRMBackend(backend), &fs.Options{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())})
			if err != nil {
				t.Fatalf("Mount: %v", err)
			}
			t.Cleanup(func() { unmountServer(t, server, mountpoint) })
			projected := filepath.Join(mountpoint, "Movie.strm")
			var heldDirectory *os.File
			if single {
				if info, err := os.Stat(projected); err == nil {
					t.Fatalf("unavailable single-file Lookup published size %d, want an error", info.Size())
				} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.EIO) {
					t.Fatalf("unavailable single-file Stat error = %v, want ENOENT or EIO", err)
				}
			} else {
				root := filepath.Join(mountpoint, "Collection")
				heldDirectory, err = os.Open(root)
				if err != nil {
					t.Fatalf("retain old torrent directory: %v", err)
				}
				t.Cleanup(func() { _ = heldDirectory.Close() })
				projected = filepath.Join(root, "Movie.strm")
				// Do not resolve a child through the deleting root: keeping this
				// directory inode alive is the stale-snapshot part of the regression.
			}
			var deletion fuseSTRMDeleteResult
			select {
			case deletion = <-backend.deleted:
			case <-ctx.Done():
				t.Fatal("listing never reached the StreamURL deletion hook")
			}
			if deletion.err != nil || deletion.operation == nil {
				t.Fatalf("delete during URL generation: %v", deletion.err)
			}
			if final := waitOperation(t, sess, deletion.operation.ID); final.State != session.StateDeleted {
				t.Fatalf("delete operation = %+v, want deleted", final)
			}
			if _, err := sess.AddTorrentAndPersist(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
				t.Fatalf("readd identical torrent: %v", err)
			}
			link, err := sess.StreamURL(hash, "Movie.mkv")
			if err != nil {
				t.Fatalf("StreamURL after readd: %v", err)
			}
			for attempt := 0; attempt < 3; attempt++ {
				fuseSTRMReadFile(t, projected, []byte(link+"\n"))
			}
			if heldDirectory != nil {
				names, err := heldDirectory.Readdirnames(-1)
				if err != nil {
					t.Fatalf("read retained directory: %v", err)
				}
				slices.Sort(names)
				if !reflect.DeepEqual(names, []string{"Movie.strm", "notes.txt"}) {
					t.Fatalf("retained directory entries = %v", names)
				}
			}
			stats := sess.RuntimeStats()
			if stats.CacheUsedBytes != 0 || stats.DownloadedBytes != 0 || stats.UploadedBytes != 0 {
				t.Fatalf("recovered STRM scan touched payload: %+v", stats)
			}
			t.Logf("real FUSE recovered %d STRM bytes after deleting and readding identical infohash", len(link)+1)
		})
	}
}
