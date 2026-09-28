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

	"github.com/yakumioto/torrentfs-go/internal/config"
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

// uploadRateWindow builds a same-day upload window that opens at the current
// local minute, plus one instant inside it and one after it. The real close
// boundary is an hour away, so the session's own scheduler cannot race the
// assertions. It skips when the local clock leaves no room on this day.
func uploadRateWindow(t *testing.T) (start, end string, inside, outside time.Time) {
	t.Helper()
	const windowMinutes = 60
	now := time.Now()
	openMinute := now.Hour()*60 + now.Minute()
	if openMinute+windowMinutes >= 24*60 {
		t.Skipf("local time %s leaves no room for a same-day upload window", now.Format("15:04"))
	}
	start = fmt.Sprintf("%02d:%02d", openMinute/60, openMinute%60)
	closeMinute := openMinute + windowMinutes
	end = fmt.Sprintf("%02d:%02d", closeMinute/60, closeMinute%60)
	inside = time.Date(now.Year(), now.Month(), now.Day(), openMinute/60, openMinute%60, 0, 0, time.Local).
		Add(20 * time.Minute)
	outside = inside.Add(windowMinutes * time.Minute)
	return start, end, inside, outside
}

// uploadRateSwarm is a loopback seeder under a rate window and an unlimited
// leecher that reads the payload it uploads.
type uploadRateSwarm struct {
	ctx            context.Context
	seeder         *session.Session
	leecher        *session.Session
	seederTorrent  *session.Torrent
	leecherTorrent *session.Torrent
	hash           metainfo.Hash
	content        []byte
	inside         time.Time
	outside        time.Time

	readDone chan error
	readBuf  []byte
}

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
	start, end, inside, outside := uploadRateWindow(t)

	seederCfg := testConfig()
	seederCfg.Upload.RateLimitBytesPerSecond = uploadRateTestBytesPerSecond
	seederCfg.Upload.Schedule = config.UploadSchedule{Start: start, End: end}
	seeder := newLivingSession(t, seederCfg, filepath.Join(work, "seeder-data"), func(cc *session.TorrentClientConfig) {
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
	// Pin the policy to a known in-window instant so the assertions do not
	// depend on when the suite runs.
	if limit, managed := seeder.ApplyUploadRatePolicyForTest(inside); !managed || limit != rate.Limit(uploadRateTestBytesPerSecond) {
		t.Fatalf("in-window limit = %v (managed=%t), want %d", limit, managed, uploadRateTestBytesPerSecond)
	}

	return &uploadRateSwarm{
		ctx:            ctx,
		seeder:         seeder,
		leecher:        leecher,
		seederTorrent:  seederTorrent,
		leecherTorrent: leecherTorrent,
		hash:           hash,
		content:        content,
		inside:         inside,
		outside:        outside,
	}
}

// startRead begins reading the whole payload through the leecher. The read
// stays pending until enough payload has been uploaded to satisfy it.
func (s *uploadRateSwarm) startRead(t *testing.T) {
	t.Helper()
	reader, err := s.leecher.OpenFile(s.hash, "payload.bin")
	if err != nil {
		t.Fatalf("leecher OpenFile: %v", err)
	}
	s.readBuf = make([]byte, len(s.content))
	s.readDone = make(chan error, 1)
	go func() {
		_, err := io.ReadFull(io.NewSectionReader(reader, 0, int64(len(s.content))), s.readBuf)
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

// TestUploadRateLimitDelaysPeerPayload proves the configured limit is enforced
// on real peer payload: a one-second sample stays well below what loopback
// delivers unlimited, and the payload is still incomplete while limited.
func TestUploadRateLimitDelaysPeerPayload(t *testing.T) {
	swarm := newUploadRateSwarm(t, 1<<20)
	swarm.startRead(t)
	swarm.waitForFirstUpload(t)

	if limit, managed := swarm.seeder.UploadRateLimitForTest(); !managed || limit != rate.Limit(uploadRateTestBytesPerSecond) {
		t.Fatalf("seeder limit during the sample = %v (managed=%t), want %d", limit, managed, uploadRateTestBytesPerSecond)
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

	if limit, _ := swarm.seeder.ApplyUploadRatePolicyForTest(swarm.outside); limit != rate.Inf {
		t.Fatalf("out-of-window limit = %v, want rate.Inf", limit)
	}
	swarm.waitRead(t)
}

// TestUploadRateScheduleSwitchKeepsUploadRunning proves a boundary switch does
// not restart or interrupt work: a read that began while limited finishes
// across the switch on the same session and torrent, with the counters still
// growing.
func TestUploadRateScheduleSwitchKeepsUploadRunning(t *testing.T) {
	swarm := newUploadRateSwarm(t, 1<<20)
	swarm.startRead(t)
	swarm.waitForFirstUpload(t)

	limited := swarm.seeder.RuntimeStats()
	if limit, managed := swarm.seeder.UploadRateLimitForTest(); !managed || limit != rate.Limit(uploadRateTestBytesPerSecond) {
		t.Fatalf("seeder limit at switch time = %v (managed=%t), want %d", limit, managed, uploadRateTestBytesPerSecond)
	}
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

	if limit, _ := swarm.seeder.ApplyUploadRatePolicyForTest(swarm.outside); limit != rate.Inf {
		t.Fatalf("out-of-window limit = %v, want rate.Inf", limit)
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
