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
	"sync"
	"syscall"
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
	waitComplete(t, ctx, st)

	server, err := filesystem.Mount(mnt, sess, nil)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(func() { _ = server.Unmount() })

	path := filepath.Join(mnt, "payload.bin", "payload.bin")
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
	waitComplete(t, ctx, st)

	server, err := filesystem.Mount(mnt, sess, nil)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(func() { unmountServer(t, server, mnt) })

	metadataDir := filepath.Join(mnt, "metadata")
	payloadDir := filepath.Join(mnt, "payload.bin")
	payloadFile := filepath.Join(payloadDir, "payload.bin")

	// Two anchor metadata files keep the churn torrent registered for the whole
	// test. Without them each create/unlink pair would add and drop the torrent
	// in anacrolix, which is slow and load-sensitive; namespace churn, not
	// torrent lifecycle, is what this test exercises.
	anchors := []string{"churn-anchor-a.torrent", "churn-anchor-b.torrent"}
	for _, name := range anchors {
		if err := os.WriteFile(filepath.Join(metadataDir, name), churnBytes, 0o644); err != nil {
			t.Fatalf("write anchor %s: %v", name, err)
		}
	}
	if err := waitFor(ctx, func() bool {
		_, registered := sess.Torrent(churnHash)
		return registered
	}); err != nil {
		t.Fatalf("churn torrent never registered: %v", err)
	}

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
				if _, err := os.Stat(payloadDir); err != nil {
					errs <- fmt.Errorf("stat torrent dir: %w", err)
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
				time.Sleep(time.Millisecond)
			}
		}()
	}

	churnDone := make(chan struct{})
	go func() {
		defer close(churnDone)
		for i := 0; i < 12; i++ {
			name := fmt.Sprintf("churn-%d.torrent", i)
			renamed := fmt.Sprintf("churn-%d-r.torrent", i)
			if err := os.WriteFile(filepath.Join(metadataDir, name), churnBytes, 0o644); err != nil {
				errs <- fmt.Errorf("write metadata %s: %w", name, err)
				return
			}
			if err := os.Rename(filepath.Join(metadataDir, name), filepath.Join(metadataDir, renamed)); err != nil {
				errs <- fmt.Errorf("rename metadata %s: %w", name, err)
				return
			}
			if err := os.Remove(filepath.Join(metadataDir, renamed)); err != nil {
				errs <- fmt.Errorf("unlink metadata %s: %w", renamed, err)
				return
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

	for _, name := range anchors {
		if err := os.Remove(filepath.Join(metadataDir, name)); err != nil {
			t.Errorf("remove anchor %s: %v", name, err)
		}
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

// TestFuseReadUnmountCloseRace races Unmount and Session.Close against read
// requests that are provably still outstanding. A test-only read gate holds
// every backend read inside ReadAt, so when the test observes a read enter the
// gate and confirms it has not returned, it knows a live FUSE request is
// pending; only then do Unmount and Session.Close start, from separate
// goroutines, and both must return within one deadline. A deadlock or data
// race that needs all three to overlap is exactly what this test catches.
func TestFuseReadUnmountCloseRace(t *testing.T) {
	requireFuse(t)
	ctx := testTimeout(t)

	work := t.TempDir()
	dataDir := filepath.Join(work, "data")
	mnt := filepath.Join(work, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatalf("make mountpoint: %v", err)
	}

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
		_ = sess.Close(context.Background())
		t.Fatalf("AddTorrent: %v", err)
	}
	st, ok := sess.Torrent(hash)
	if !ok {
		_ = sess.Close(context.Background())
		t.Fatal("torrent not registered")
	}
	waitComplete(t, ctx, st)

	server, err := filesystem.Mount(mnt, sess, nil)
	if err != nil {
		_ = sess.Close(context.Background())
		t.Fatalf("Mount: %v", err)
	}
	defer unmountServer(t, server, mnt)

	path := filepath.Join(mnt, "payload.bin", "payload.bin")

	// Hold every backend read inside ReadAt until released. A reader blocked
	// here has entered ReadAt and not returned, so its FUSE request is
	// outstanding by construction rather than by timing.
	readEntered := make(chan struct{})
	readRelease := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	releaseReads := func() { releaseOnce.Do(func() { close(readRelease) }) }
	restoreGate := session.SetReadGate(func() {
		enteredOnce.Do(func() { close(readEntered) })
		<-readRelease
	})
	// A failed assertion anywhere below must not leave the hook installed or
	// the readers blocked on the gate: release them, then clear the gate.
	defer func() {
		releaseReads()
		restoreGate()
	}()

	const readers = 3
	readerDone := make(chan error, readers)
	var readerWG sync.WaitGroup
	for i := 0; i < readers; i++ {
		readerWG.Add(1)
		go func(seed int) {
			defer readerWG.Done()
			f, err := os.Open(path)
			if err != nil {
				readerDone <- fmt.Errorf("open: %w", err)
				return
			}
			defer func() { _ = f.Close() }()
			buf := make([]byte, concurrencyReadChunk)
			off := int64(seed * concurrencyReadChunk % (len(content) - concurrencyReadChunk))
			if _, err := f.ReadAt(buf, off); err != nil {
				readerDone <- err
				return
			}
			readerDone <- nil
		}(i)
	}

	select {
	case <-readEntered:
	case <-ctx.Done():
		t.Fatal("no read ever entered the gate")
	}
	// The gate is closed, so a read is inside ReadAt; confirm none has
	// returned yet. Together these prove a live, unfinished read exists now.
	select {
	case err := <-readerDone:
		t.Fatalf("a reader finished before Unmount/Close started: %v", err)
	default:
	}

	unmountDone := make(chan error, 1)
	closeDone := make(chan error, 1)
	go func() { unmountDone <- server.Unmount() }()
	go func() { closeDone <- sess.Close(ctx) }()

	// Let both calls overlap the outstanding reads, then release them so the
	// kernel can drain the requests and drop the mount.
	time.Sleep(50 * time.Millisecond)
	releaseReads()

	var unmountErr, closeErr error
	deadline := time.After(30 * time.Second)
	for pending := 2; pending > 0; {
		select {
		case unmountErr = <-unmountDone:
			pending--
		case closeErr = <-closeDone:
			pending--
		case <-deadline:
			t.Fatal("Unmount and Session.Close did not both return: deadlock")
		}
	}
	if closeErr != nil {
		t.Errorf("Session.Close: %v", closeErr)
	}
	if unmountErr != nil {
		t.Errorf("Unmount while reads were outstanding: %v", unmountErr)
	}

	waitGroupWithin(t, ctx, &readerWG, "outstanding readers")
	for i := 0; i < readers; i++ {
		// The reads were released only after Unmount and Close had started, so
		// each must fail. The exact errno varies with which of the two tore the
		// mount down first (EIO from the FUSE layer, EBADF from the closed
		// session); what matters is that none reports success.
		if err := <-readerDone; err == nil {
			t.Error("a read completed successfully after Unmount and Session.Close; want an error")
		}
	}

	// The mount is gone, so a fresh open must fail.
	if f, err := os.Open(path); err == nil {
		_ = f.Close()
		t.Error("open after Unmount and Close succeeded; want an error")
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
	waitComplete(t, ctx, st)

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
// metadata mutations issued after Close map to the project's ErrClosed.
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
	if _, err := sess.PieceStates(hash); !errors.Is(err, filesystem.ErrClosed) {
		t.Errorf("PieceStates after Close = %v, want ErrClosed", err)
	}
	if got := sess.Torrents(); got != nil {
		t.Errorf("Torrents after Close = %v, want nil", got)
	}
	if _, err := sess.BeginMetadata(ctx, "late.torrent", uint32(syscall.O_WRONLY)); !errors.Is(err, filesystem.ErrClosed) {
		t.Errorf("BeginMetadata after Close = %v, want ErrClosed", err)
	}
	if err := sess.RemoveMetadata(ctx, "late.torrent"); !errors.Is(err, filesystem.ErrClosed) {
		t.Errorf("RemoveMetadata after Close = %v, want ErrClosed", err)
	}
}
