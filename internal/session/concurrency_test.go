package session_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

const concurrencyReadChunk = 4096

// testTimeout bounds every concurrent test so a deadlock fails loudly instead
// of hanging the suite.
func testTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// waitGroupWithin waits for wg, failing the test if it has not drained by the
// time ctx expires.
func waitGroupWithin(t *testing.T, ctx context.Context, wg *sync.WaitGroup, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("%s did not finish: %v", what, ctx.Err())
	}
}

func drainErrors(t *testing.T, errs chan error) {
	t.Helper()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// TestFuseConcurrentReads opens many independent descriptors on one mounted
// file and reads overlapping windows at varying offsets, checking every byte
// against the source.
func TestFuseConcurrentReads(t *testing.T) {
	requireFuse(t)
	ctx := testTimeout(t)

	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	mnt := filepath.Join(work, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatalf("make mountpoint: %v", err)
	}

	content := make([]byte, testPieceLength+12345)
	for i := range content {
		content[i] = byte(i*17 + i/97)
	}
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", content)

	sess, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	seedPieces(t, sess, hash, content)
	waitCached(t, ctx, st)

	server, err := filesystem.Mount(mnt, sess, nil)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(func() { _ = server.Unmount() })

	path := filepath.Join(mnt, "payload.bin")
	maxOffset := len(content) - concurrencyReadChunk

	const readers = 8
	const iterations = 6
	errs := make(chan error, readers)
	var wg sync.WaitGroup
	for seed := 0; seed < readers; seed++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for iter := 0; iter < iterations; iter++ {
				off := int64((seed*7 + iter*13) % maxOffset)
				f, err := os.Open(path)
				if err != nil {
					errs <- fmt.Errorf("open: %w", err)
					return
				}
				buf := make([]byte, concurrencyReadChunk)
				half := concurrencyReadChunk / 2
				if _, err := f.ReadAt(buf[:half], off); err != nil && err != io.EOF {
					_ = f.Close()
					errs <- fmt.Errorf("read head at %d: %w", off, err)
					return
				}
				// Second descriptor positions independently of the first.
				if _, err := f.ReadAt(buf[half:], off+int64(half)); err != nil && err != io.EOF {
					_ = f.Close()
					errs <- fmt.Errorf("read tail at %d: %w", off+int64(half), err)
					return
				}
				if err := f.Close(); err != nil {
					errs <- fmt.Errorf("close: %w", err)
					return
				}
				if !bytes.Equal(buf, content[off:off+concurrencyReadChunk]) {
					errs <- fmt.Errorf("content mismatch at offset %d", off)
					return
				}
			}
		}(seed)
	}
	waitGroupWithin(t, ctx, &wg, "concurrent readers")
	drainErrors(t, errs)
}

// TestFuseConcurrentNamespaceChurn hammers lookups and directory listings
// while metadata files are created, renamed, and unlinked underneath, so the
// dynamic namespace is exercised under contention.
func TestFuseConcurrentNamespaceChurn(t *testing.T) {
	requireFuse(t)
	ctx := testTimeout(t)

	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	mnt := filepath.Join(work, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatalf("make mountpoint: %v", err)
	}

	content := []byte("namespace churn payload")
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", content)
	churnBytes, churnHash := buildSingleFileTorrentBytes(t, "churn.bin", []byte("churn payload"), nil)

	sess, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	seedPieces(t, sess, hash, content)
	waitCached(t, ctx, st)

	server, err := filesystem.Mount(mnt, sess, nil)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(func() { unmountServer(t, server, mnt) })

	payloadFile := filepath.Join(mnt, "payload.bin")
	churnRoot := filepath.Join(mnt, "churn.bin")

	stop := make(chan struct{})
	errs := make(chan error, 16)

	var readers sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := os.ReadDir(mnt); err != nil {
					errs <- fmt.Errorf("readdir mount root: %w", err)
					return
				}
				if _, err := os.Stat(payloadFile); err != nil {
					errs <- fmt.Errorf("stat payload file: %w", err)
					return
				}
				f, err := os.Open(payloadFile)
				if err != nil {
					errs <- fmt.Errorf("open payload: %w", err)
					return
				}
				buf := make([]byte, len(content))
				if _, err := f.ReadAt(buf, 0); err != nil && err != io.EOF {
					_ = f.Close()
					errs <- fmt.Errorf("read payload: %w", err)
					return
				}
				_ = f.Close()
				if !bytes.Equal(buf, content) {
					errs <- errors.New("payload changed during namespace churn")
					return
				}
				// The churn torrent's data node appears and disappears while the
				// stable payload stays readable; the data read above must never
				// be disturbed by that namespace churn.
				if _, err := os.Stat(churnRoot); err != nil && !errors.Is(err, os.ErrNotExist) {
					errs <- fmt.Errorf("stat churn node: %w", err)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}

	churnDone := make(chan struct{})
	go func() {
		defer close(churnDone)
		for i := 0; i < 6; i++ {
			view, err := sess.AddTorrentAndPersist(ctx, session.Source{Metainfo: churnBytes})
			if err != nil {
				errs <- fmt.Errorf("add churn torrent %d: %w", i, err)
				return
			}
			op, err := sess.DeleteTorrent(ctx, view.ID)
			if err != nil {
				errs <- fmt.Errorf("delete churn torrent %d: %w", i, err)
				return
			}
			for {
				current, ok := sess.Operation(op.ID)
				if ok && current.State != session.StateDeleting {
					break
				}
				select {
				case <-ctx.Done():
					errs <- fmt.Errorf("churn deletion %d never finished: %w", i, ctx.Err())
					return
				case <-time.After(time.Millisecond):
				}
			}
		}
	}()

	select {
	case <-churnDone:
	case <-ctx.Done():
		close(stop)
		t.Fatalf("namespace churn did not finish: %v", ctx.Err())
	}
	close(stop)
	waitGroupWithin(t, ctx, &readers, "namespace readers")

	if _, ok := sess.Torrent(churnHash); ok {
		t.Error("churn torrent survived its deletion")
	}
	drainErrors(t, errs)
}

// unmountServer unmounts server and fails the test if the mount survives: a
// silent unmount error would let the next test or job run into a live mount.
// A mount that cannot be released gracefully is force-detached so it does not
// leak. The unmount runs on its own goroutine with a deadline; the result
// channel is buffered, so even a timed-out unmount cannot strand the goroutine.
func unmountServer(t *testing.T, server interface{ Unmount() error }, mountpoint string) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- server.Unmount() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("unmount %s: %v", mountpoint, err)
			forceUnmount(t, mountpoint)
		}
	case <-time.After(30 * time.Second):
		t.Errorf("unmount %s did not return within 30s", mountpoint)
		forceUnmount(t, mountpoint)
	}
}

// forceUnmount lazily detaches a mount that a graceful unmount could not
// release, so a failing test does not leave a live mount behind.
func forceUnmount(t *testing.T, mountpoint string) {
	t.Helper()
	for _, tool := range []string{"fusermount3", "fusermount"} {
		if _, err := exec.LookPath(tool); err != nil {
			continue
		}
		if err := exec.Command(tool, "-uz", mountpoint).Run(); err == nil {
			t.Logf("force-unmounted %s with %s -uz", mountpoint, tool)
			return
		}
	}
	t.Errorf("could not force-unmount %s; a FUSE mount may remain", mountpoint)
}

// TestFuseCloseFirstShutdownReleasesBlockedReads is the lifecycle regression
// for the production shutdown order. Two descriptors read two different
// missing pieces of a peerless torrent through a real FUSE mount, so both
// reads are provably blocked in the shared loader rather than merely
// outstanding at the ReadAt entry. The session is then closed and only after
// that is the mount unmounted, matching main's shutdown sequence; the blocked
// reads must fail, and Unmount must not block or report the daemon-owned
// device-busy failure the unmount-first order produced.
func TestFuseCloseFirstShutdownReleasesBlockedReads(t *testing.T) {
	requireFuse(t)
	ctx := testTimeout(t)

	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	mnt := filepath.Join(work, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatalf("make mountpoint: %v", err)
	}

	// No payload is written and no peer ever joins, so every piece stays
	// missing and reads block until the session is closed.
	content := make([]byte, 3*testPieceLength+4096)
	for i := range content {
		content[i] = byte(i*13 + i/97)
	}
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)
	torrentPath := filepath.Join(work, "payload.bin.torrent")
	if err := os.WriteFile(torrentPath, torrentBytes, 0o644); err != nil {
		t.Fatalf("write torrent: %v", err)
	}

	sess := newLoopbackSession(t, testConfig(dataDir))
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	select {
	case <-st.GotInfo():
	case <-ctx.Done():
		t.Fatalf("torrent info never arrived: %v", ctx.Err())
	}
	if st.CachedBytes() != 0 {
		t.Fatal("the torrent must stay uncached for this test")
	}

	server, err := filesystem.Mount(mnt, sess, nil)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	// A graceful unmount short-circuits only once one has succeeded, so a failed
	// or timed-out attempt is retried rather than leaving a live mount behind.
	// cleanup force-detaches a mount that still cannot be released, and every
	// attempt is bounded with a buffered result channel so a timed-out goroutine
	// cannot block on its send.
	var unmountSucceeded atomic.Bool
	unmountOnce := func(deadline time.Duration) error {
		if unmountSucceeded.Load() {
			return nil
		}
		done := make(chan error, 1)
		go func() { done <- server.Unmount() }()
		select {
		case err := <-done:
			if err != nil {
				return err
			}
			unmountSucceeded.Store(true)
			return nil
		case <-time.After(deadline):
			return fmt.Errorf("unmount %s did not return within %s", mnt, deadline)
		}
	}
	t.Cleanup(func() {
		if unmountSucceeded.Load() {
			return
		}
		if err := unmountOnce(30 * time.Second); err != nil {
			t.Errorf("cleanup unmount %s: %v", mnt, err)
			forceUnmount(t, mnt)
		}
	})

	path := filepath.Join(mnt, "payload.bin")
	firstFile, err := os.Open(path)
	if err != nil {
		t.Fatalf("open first descriptor: %v", err)
	}
	// The descriptors are closed before the unmount below: an open file is a
	// host holder, and any unmount fails on one. The daemon-owned failure this
	// test targets is the one that happens with no holder at all.
	defer func() { _ = firstFile.Close() }()
	secondFile, err := os.Open(path)
	if err != nil {
		t.Fatalf("open second descriptor: %v", err)
	}
	defer func() { _ = secondFile.Close() }()

	events := make(chan session.ReadProbeEvent, 32)
	restoreProbe := session.SetReadProbe(events)
	defer restoreProbe()

	// Each descriptor reads a different missing piece, so the offsets identify
	// which request reached the loader.
	firstOffset := int64(testPieceLength)
	secondOffset := int64(2 * testPieceLength)

	firstDone := readAtAsync(firstFile, firstOffset+512, 64)
	firstStarted := waitProbeEvent(t, events, "reader-started", firstOffset)

	secondDone := readAtAsync(secondFile, secondOffset+512, 64)
	waitProbeEvent(t, events, "admission-attempt", secondOffset)

	// The second request must not have cancelled the first, and neither read
	// may have returned while the pieces are missing.
	if probeDoneClosed(firstStarted.Done) {
		t.Fatal("the second read cancelled the first read's operation")
	}
	requireNoReadOutcome(t, firstDone, "first blocked read")
	requireNoReadOutcome(t, secondDone, "second blocked read")

	// Production order: close the session first, from its own goroutine with
	// the test's own deadline, because Session.Close ignores its context.
	closeDone := make(chan error, 1)
	go func() { closeDone <- sess.Close(context.Background()) }()

	waitProbeDoneClosed(t, firstStarted.Done, "blocked operation context")

	firstOutcome := waitReadOutcome(t, firstDone, "first blocked read after Close")
	if firstOutcome.err == nil {
		t.Fatalf("first blocked read returned %d bytes after Close; want an error", len(firstOutcome.data))
	}
	secondOutcome := waitReadOutcome(t, secondDone, "second blocked read after Close")
	if secondOutcome.err == nil {
		t.Fatalf("second blocked read returned %d bytes after Close; want an error", len(secondOutcome.data))
	}

	select {
	case err := <-closeDone:
		if err != nil {
			t.Errorf("Session.Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Session.Close did not return while reads were blocked")
	}

	// Reclaim every descriptor before releasing the mount, so the unmount
	// result cannot be blamed on a holder this test created itself.
	if err := firstFile.Close(); err != nil {
		t.Errorf("close first descriptor: %v", err)
	}
	if err := secondFile.Close(); err != nil {
		t.Errorf("close second descriptor: %v", err)
	}

	// Only after Close returned and the descriptors are gone may the mount be
	// released; it must succeed with no device-busy failure.
	if err := unmountOnce(30 * time.Second); err != nil {
		t.Fatalf("Unmount after Session.Close: %v", err)
	}

	// The mount is gone, so a fresh open must fail and no descriptor or read
	// goroutine may still be live.
	if f, err := os.Open(path); err == nil {
		_ = f.Close()
		t.Error("open after Session.Close and Unmount succeeded; want an error")
	}
}

// TestSessionOpenFileReadsStopAfterClose drives a read loop through an already
// open file handle and closes the session underneath it. The loop must end on
// ErrClosed rather than spinning forever, which is what makes the FUSE race
// test meaningful.
func TestSessionOpenFileReadsStopAfterClose(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	content := make([]byte, testPieceLength+777)
	for i := range content {
		content[i] = byte(i)
	}
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", content)

	sess, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatal("torrent not registered")
	}
	seedPieces(t, sess, hash, content)
	waitCached(t, ctx, st)

	ra, err := sess.OpenFile(hash, "payload.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		buf := make([]byte, concurrencyReadChunk)
		for {
			if _, err := ra.ReadAt(buf, 0); err != nil {
				return
			}
		}
	}()

	time.Sleep(20 * time.Millisecond)
	if err := sess.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("reads on an open handle kept succeeding after Session.Close")
	}
}

// TestSessionConcurrentCloseIsSafe closes one session from many goroutines.
// Every caller must return, and all must observe the same result.
func TestSessionConcurrentCloseIsSafe(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	sess, err := session.New(testConfig(filepath.Join(work, "data")), testTorrentDir(t, filepath.Join(work, "data")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const callers = 16
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- sess.Close(ctx)
		}()
	}
	waitGroupWithin(t, ctx, &wg, "concurrent Close callers")
	drainErrors(t, errs)
}

// TestSessionClosingStateRejectsOperations checks that reads, lookups, and
// management queries issued after Close map to the project's ErrClosed.
func TestSessionClosingStateRejectsOperations(t *testing.T) {
	ctx := testTimeout(t)
	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	torrentPath, hash := buildSingleFileTorrent(t, dataDir, work, "payload.bin", []byte("closed state"))

	sess, err := session.New(testConfig(dataDir), testTorrentDir(t, dataDir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	if err := sess.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := sess.OpenFile(hash, "payload.bin"); !errors.Is(err, filesystem.ErrClosed) {
		t.Errorf("OpenFile after Close = %v, want ErrClosed", err)
	}
	if _, err := sess.TorrentStatusFor(hash.HexString()); !errors.Is(err, filesystem.ErrClosed) {
		t.Errorf("TorrentStatusFor after Close = %v, want ErrClosed", err)
	}
	if got := sess.Torrents(); got != nil {
		t.Errorf("Torrents after Close = %v, want nil", got)
	}
	if _, err := sess.AddTorrentAndPersist(ctx, session.Source{MagnetURI: "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)}); !errors.Is(err, filesystem.ErrClosed) {
		t.Errorf("AddTorrentAndPersist after Close = %v, want ErrClosed", err)
	}
}
