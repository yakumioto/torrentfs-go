package session_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"golang.org/x/time/rate"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

// The loopback swarm stays small and the rate low enough that a limited
// transfer is unmistakably slower than an unlimited one, while the payload is
// still large enough to take several seconds. The burst sits just above one
// request chunk: the client's default burst would let this payload through
// before the rate ever mattered.
const (
	uploadRateTestBytesPerSecond = 64 << 10
	uploadRateTestBurstBytes     = 64 << 10
)

// uploadRateWindows returns one same-day schedule that contains the current
// minute and one that already closed, both built from the running clock so the
// assertions never depend on when the suite runs. It skips when the local clock
// leaves no room for either window on this day.
func uploadRateWindows(t *testing.T) (open, closed *session.UploadRateSchedule) {
	t.Helper()
	const windowMinutes = 60
	now := time.Now()
	minute := now.Hour()*60 + now.Minute()
	if minute < windowMinutes || minute+windowMinutes >= 24*60 {
		t.Skipf("local time %s leaves no room for both same-day upload windows", now.Format("15:04"))
	}
	format := func(value int) string { return fmt.Sprintf("%02d:%02d", value/60, value%60) }
	open = &session.UploadRateSchedule{Start: format(minute), End: format(minute + windowMinutes)}
	closed = &session.UploadRateSchedule{Start: format(minute - windowMinutes), End: format(minute)}
	return open, closed
}

// uploadRateSwarm is a loopback seeder under a persisted upload-limit window and
// an unlimited leecher that reads the payload it uploads.
type uploadRateSwarm struct {
	ctx            context.Context
	seeder         *session.Session
	leecher        *session.Session
	seederTorrent  *session.Torrent
	leecherTorrent *session.Torrent
	hash           metainfo.Hash
	content        []byte

	openWindow   *session.UploadRateSchedule
	closedWindow *session.UploadRateSchedule

	readDone chan error
	readBuf  []byte
}

// newUploadRateSwarm seeds pieces on the seeder and installs the limiting window
// through the public settings API, so the persistence and application paths are
// the same ones the Web UI uses.
func newUploadRateSwarm(t *testing.T, size int) *uploadRateSwarm {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	work := t.TempDir()
	tracker := newLoopbackTracker(t)
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i*31 + i/251)
	}
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, [][]string{{tracker.url}})
	hashHex := hash.HexString()
	open, closed := uploadRateWindows(t)

	seeder := newLivingSession(t, testConfig(), filepath.Join(work, "seeder-data"), func(cc *session.TorrentClientConfig) {
		// A small burst keeps a limited transfer slow enough to observe while
		// still fitting one request chunk.
		cc.UploadRateLimiter.SetBurst(uploadRateTestBurstBytes)
	})
	leecher := newLivingSession(t, testConfig(), filepath.Join(work, "leecher-data"), nil)
	t.Cleanup(func() {
		if err := leecher.Close(context.Background()); err != nil {
			t.Errorf("close leecher: %v", err)
		}
		if err := seeder.Close(context.Background()); err != nil {
			t.Errorf("close seeder: %v", err)
		}
	})

	if _, err := seeder.SetUploadRateSettings(ctx, session.UploadRateSettings{
		RateLimitBytesPerSecond: uploadRateTestBytesPerSecond,
		Schedule:                open,
	}); err != nil {
		t.Fatalf("SetUploadRateSettings: %v", err)
	}

	if err := seeder.AddTorrent(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("seeder AddTorrent: %v", err)
	}
	seederTorrent, ok := seeder.Torrent(hash)
	if !ok {
		t.Fatal("seeder did not register the torrent")
	}
	seedPieces(t, seeder, hash, content)
	waitCached(t, ctx, seederTorrent)

	if err := leecher.AddTorrent(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("leecher AddTorrent: %v", err)
	}
	leecherTorrent, ok := leecher.Torrent(hash)
	if !ok {
		t.Fatal("leecher did not register the torrent")
	}
	if err := waitFor(ctx, func() bool { return tracker.peerCount(hashHex) >= 2 }); err != nil {
		t.Fatalf("seeder and leecher never connected: %v", err)
	}
	if limit, _ := seeder.UploadRateLimitForTest(); limit != rate.Limit(uploadRateTestBytesPerSecond) {
		t.Fatalf("in-window limit = %v, want %d", limit, uploadRateTestBytesPerSecond)
	}

	return &uploadRateSwarm{
		ctx:            ctx,
		seeder:         seeder,
		leecher:        leecher,
		seederTorrent:  seederTorrent,
		leecherTorrent: leecherTorrent,
		hash:           hash,
		content:        content,
		openWindow:     open,
		closedWindow:   closed,
	}
}

// startRead begins reading the whole payload through the leecher. The read stays
// pending until enough payload has been uploaded to satisfy it.
func (s *uploadRateSwarm) startRead(t *testing.T) {
	t.Helper()
	reader, err := s.leecher.OpenFile(s.hash, "payload.bin")
	if err != nil {
		t.Fatalf("leecher OpenFile: %v", err)
	}
	s.readBuf = make([]byte, int(s.leecherTorrent.Length()))
	s.readDone = make(chan error, 1)
	go func() {
		_, err := io.ReadFull(io.NewSectionReader(reader, 0, int64(len(s.readBuf))), s.readBuf)
		if err == nil && !bytes.Equal(s.readBuf, s.content) {
			err = errors.New("read payload differs from the seeder content")
		}
		s.readDone <- err
	}()
}

func (s *uploadRateSwarm) waitRead(t *testing.T) {
	t.Helper()
	select {
	case err := <-s.readDone:
		if err != nil {
			t.Fatalf("leecher payload read: %v", err)
		}
	case <-s.ctx.Done():
		t.Fatalf("leecher payload read did not finish: %v", s.ctx.Err())
	}
}

func (s *uploadRateSwarm) waitForFirstUpload(t *testing.T) {
	t.Helper()
	if err := waitFor(s.ctx, func() bool { return s.seeder.RuntimeStats().UploadedBytes > 0 }); err != nil {
		t.Fatalf("seeder never uploaded payload: %v", err)
	}
}

// TestUploadRateLimitDelaysPeerPayload proves the persisted limit is enforced on
// real peer payload: a one-second sample stays well below what loopback delivers
// unlimited, and the payload is still incomplete while limited.
func TestUploadRateLimitDelaysPeerPayload(t *testing.T) {
	swarm := newUploadRateSwarm(t, 1<<20)
	swarm.startRead(t)
	swarm.waitForFirstUpload(t)

	if limit, _ := swarm.seeder.UploadRateLimitForTest(); limit != rate.Limit(uploadRateTestBytesPerSecond) {
		t.Fatalf("seeder limit during the sample = %v, want %d", limit, uploadRateTestBytesPerSecond)
	}
	before := swarm.seeder.RuntimeStats().UploadedBytes
	time.Sleep(time.Second)
	after := swarm.seeder.RuntimeStats().UploadedBytes
	if after <= before {
		t.Fatalf("upload stalled while limited: %d -> %d bytes", before, after)
	}
	if grew := after - before; grew > 3*uploadRateTestBytesPerSecond {
		t.Fatalf("seeder uploaded %d bytes in one second under a %d B/s limit; the limit is not in effect", grew, uploadRateTestBytesPerSecond)
	}
	if cached := swarm.leecherTorrent.CachedBytes(); cached >= swarm.leecherTorrent.Length() {
		t.Fatalf("the whole payload transferred while limited (%d bytes)", cached)
	}

	// Turning the limit off through the settings API is the same path the Web UI
	// takes, and it must release the upload without restarting anything.
	if _, err := swarm.seeder.SetUploadRateSettings(swarm.ctx, session.UploadRateSettings{}); err != nil {
		t.Fatalf("disable upload limiting: %v", err)
	}
	if limit, _ := swarm.seeder.UploadRateLimitForTest(); limit != rate.Inf {
		t.Fatalf("limit after disabling = %v, want rate.Inf", limit)
	}
	swarm.waitRead(t)
}

// TestUploadRateScheduleSwitchKeepsUploadRunning proves a window change does not
// restart or interrupt work: a read that began while limited finishes across the
// switch on the same session and torrent, with the counters still growing and
// the new window persisted.
func TestUploadRateScheduleSwitchKeepsUploadRunning(t *testing.T) {
	swarm := newUploadRateSwarm(t, 1<<20)
	swarm.startRead(t)
	swarm.waitForFirstUpload(t)

	limited := swarm.seeder.RuntimeStats()
	if int64(len(swarm.content)) <= limited.UploadedBytes {
		t.Fatalf("seeder had already uploaded the whole payload (%d bytes) before the switch", limited.UploadedBytes)
	}
	seederTorrent := session.UnderlyingTorrentForTest(swarm.seederTorrent)
	leecherTorrent := session.UnderlyingTorrentForTest(swarm.leecherTorrent)
	select {
	case err := <-swarm.readDone:
		t.Fatalf("the read finished while still limited: %v", err)
	default:
	}

	// The new window closed at the current minute, so the payload is unlimited
	// from now on while the schedule stays configured.
	applied, err := swarm.seeder.SetUploadRateSettings(swarm.ctx, session.UploadRateSettings{
		RateLimitBytesPerSecond: uploadRateTestBytesPerSecond,
		Schedule:                swarm.closedWindow,
	})
	if err != nil {
		t.Fatalf("SetUploadRateSettings: %v", err)
	}
	if applied.Schedule == nil || applied.Schedule.End != swarm.closedWindow.End {
		t.Fatalf("applied settings = %+v, want the closed window", applied)
	}
	if limit, _ := swarm.seeder.UploadRateLimitForTest(); limit != rate.Inf {
		t.Fatalf("limit outside the window = %v, want rate.Inf", limit)
	}
	swarm.waitRead(t)

	after := swarm.seeder.RuntimeStats()
	if after.UploadedBytes <= limited.UploadedBytes {
		t.Fatalf("upload counters did not grow across the switch: %d -> %d", limited.UploadedBytes, after.UploadedBytes)
	}
	if !after.StartedAt.Equal(limited.StartedAt) {
		t.Fatalf("seeder restarted across the switch: %s -> %s", limited.StartedAt, after.StartedAt)
	}
	if session.UnderlyingTorrentForTest(swarm.seederTorrent) != seederTorrent {
		t.Fatal("seeder torrent was replaced across the boundary")
	}
	if session.UnderlyingTorrentForTest(swarm.leecherTorrent) != leecherTorrent {
		t.Fatal("leecher torrent was replaced across the boundary")
	}
	if cached := swarm.leecherTorrent.CachedBytes(); cached != swarm.leecherTorrent.Length() {
		t.Fatalf("leecher cached %d/%d bytes after the read", cached, swarm.leecherTorrent.Length())
	}
}
