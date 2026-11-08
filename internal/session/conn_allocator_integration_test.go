package session_test

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

// seederFor adds one torrent to a fresh loopback session, seeds the content and
// waits until the allocator has opened its probe slot, so the leecher's dial is
// accepted.
func seederFor(t *testing.T, ctx context.Context, dir string, torrentBytes []byte, hash metainfo.Hash, content []byte) *session.Session {
	t.Helper()
	seeder := newLoopbackSessionWithCustomize(t, testConfig(), dir,
		func(cc *session.TorrentClientConfig) { cc.DropMutuallyCompletePeers = false })
	if err := seeder.AddTorrent(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("seeder AddTorrent: %v", err)
	}
	state, ok := seeder.Torrent(hash)
	if !ok {
		t.Fatal("seeder did not register the torrent")
	}
	seedPieces(t, seeder, hash, content)
	waitCached(t, ctx, state)
	if err := waitFor(ctx, func() bool { return session.ConnectionAllowanceForTest(state) >= 1 }); err != nil {
		t.Fatalf("the seeder never got its probe connection allowance: %v", err)
	}
	return seeder
}

// fakeConnectionClock installs a mutable allocator clock on the session and
// returns an advance function, so grace and rotation windows can be walked
// without sleeping.
func fakeConnectionClock(t *testing.T, sess *session.Session) (advance func(time.Duration)) {
	t.Helper()
	var nanoseconds atomic.Int64
	nanoseconds.Store(time.Now().UnixNano())
	restore := session.SetConnectionAllocatorClockForTest(sess, func() time.Time {
		return time.Unix(0, nanoseconds.Load())
	})
	t.Cleanup(restore)
	return func(d time.Duration) { nanoseconds.Add(int64(d)) }
}

// TestConnectionAllocatorFollowsReadActivity is the end-to-end contract of the
// activity-based allowances: a torrent opens no peer connections while idle,
// connects to more than one peer when it is read, and afterwards falls back to
// the single probe connection while dropping the excess. The swarm is wired
// with explicit peer addresses instead of a tracker, so the seeders cannot
// spend their one probe slot on each other before the leecher arrives.
func TestConnectionAllocatorFollowsReadActivity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	work := t.TempDir()
	content := bytes.Repeat([]byte{7}, 8*testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, nil)

	const seederCount = 3
	seederAddrs := make([]torrent.PeerInfo, 0, seederCount)
	for i := 0; i < seederCount; i++ {
		seeder := seederFor(t, ctx, filepath.Join(work, fmt.Sprintf("seeder-%d", i)), torrentBytes, hash, content)
		seederAddrs = append(seederAddrs, torrent.PeerInfo{
			Addr: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(seeder.EffectiveListenPort())),
		})
	}

	cfg := testConfig()
	leecher := newLoopbackSessionWithCustomize(t, cfg, filepath.Join(work, "leecher"),
		func(cc *session.TorrentClientConfig) { cc.DropMutuallyCompletePeers = false })
	advance := fakeConnectionClock(t, leecher)
	if err := leecher.AddTorrent(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("leecher AddTorrent: %v", err)
	}
	state, ok := leecher.Torrent(hash)
	if !ok {
		t.Fatal("leecher did not register the torrent")
	}

	// Idle: no reader and no cached pieces, so the torrent may not connect.
	session.AllocateConnectionsForTest(leecher)
	if got := session.ConnectionAllowanceForTest(state); got != 0 {
		t.Fatalf("a new idle torrent has allowance %d, want 0", got)
	}
	if got := session.ConnectionStateForTest(state); got != "idle" {
		t.Fatalf("a new idle torrent is in state %q, want idle", got)
	}
	if peers := totalActivePeers(leecher); peers != 0 {
		t.Fatalf("an idle torrent holds %d peer connections, want 0", peers)
	}

	// Reading grants the reading allowance. Add the seeder addresses after the
	// allowance exists so the resulting connections are established.
	path := session.FirstFilePathForTest(state)
	if path == "" {
		t.Fatal("the torrent has no file to read")
	}
	reader, closer, err := session.OpenTorrentFileForTest(state, path)
	if err != nil {
		t.Fatalf("open the torrent file: %v", err)
	}
	session.AllocateConnectionsForTest(leecher)
	if got := session.ConnectionAllowanceForTest(state); got != cfg.Connections.ReadingConns {
		t.Fatalf("a reading torrent has allowance %d, want %d", got, cfg.Connections.ReadingConns)
	}
	if got := session.ConnectionStateForTest(state); got != "reading" {
		t.Fatalf("a reading torrent is in state %q, want reading", got)
	}
	session.UnderlyingTorrentForTest(state).AddPeers(seederAddrs)
	if _, err := reader.ReadAt(make([]byte, 32<<10), 0); err != nil {
		t.Fatalf("read the torrent file: %v", err)
	}
	if err := waitFor(ctx, func() bool { return totalActivePeers(leecher) >= 2 }); err != nil {
		t.Fatalf("the reading torrent never connected to more than one seeder: %v", err)
	}

	// Closing the handle starts the grace window. Once it expires the torrent
	// drops to the single probe connection and the excess connections go away.
	if err := closer.Close(); err != nil {
		t.Fatalf("close the torrent file: %v", err)
	}
	advance(time.Duration(cfg.Connections.ReadGrace) + time.Second)
	session.AllocateConnectionsForTest(leecher)
	if got := session.ConnectionAllowanceForTest(state); got != 1 {
		t.Fatalf("a quiet cached torrent has allowance %d, want the probe allowance 1", got)
	}
	if got := session.ConnectionStateForTest(state); got != "probe" {
		t.Fatalf("a quiet cached torrent is in state %q, want probe", got)
	}
	if err := waitFor(ctx, func() bool { return totalActivePeers(leecher) <= 1 }); err != nil {
		t.Fatalf("the quiet torrent kept more than the probe connection: %v", err)
	}
}

// TestConnectionAllocatorFetchesMagnetMetadata covers the magnet regression: a
// magnet has no metadata and no cache, so only the metadata-stage allowance can
// connect it to a peer. Once the metadata arrives the torrent falls back to the
// idle allowance.
func TestConnectionAllocatorFetchesMagnetMetadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	work := t.TempDir()
	tracker := newLoopbackTracker(t)
	content := bytes.Repeat([]byte{9}, 8*testPieceLength)
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, [][]string{{tracker.url}})
	seederFor(t, ctx, filepath.Join(work, "seeder"), torrentBytes, hash, content)

	magnetURI := metainfo.Magnet{
		InfoHash:    hash,
		Trackers:    []string{tracker.url},
		DisplayName: "payload.bin",
	}.String()

	leecher := newLoopbackSessionWithCustomize(t, testConfig(), filepath.Join(work, "leecher"), nil)
	if err := leecher.AddTorrent(ctx, session.Source{MagnetURI: magnetURI}); err != nil {
		t.Fatalf("leecher AddTorrent magnet: %v", err)
	}
	state, ok := leecher.Torrent(hash)
	if !ok {
		t.Fatal("leecher did not register the magnet")
	}

	// The magnet has no metadata yet, so it must be in the metadata stage with
	// the metadata allowance rather than pinned at zero.
	if info := state.Info(); info != nil {
		t.Fatal("the magnet already has metadata before the first allocation")
	}
	session.AllocateConnectionsForTest(leecher)
	if got := session.ConnectionStateForTest(state); got != "metadata" {
		t.Fatalf("a magnet without metadata is in state %q, want metadata", got)
	}
	if got := session.ConnectionAllowanceForTest(state); got != session.MetadataStageConnectionsForTest() {
		t.Fatalf("a magnet without metadata has allowance %d, want %d", got, session.MetadataStageConnectionsForTest())
	}

	select {
	case <-state.GotInfo():
	case <-ctx.Done():
		t.Fatal("the magnet never fetched its metadata from the seeder")
	}
	session.AllocateConnectionsForTest(leecher)
	if got := session.ConnectionStateForTest(state); got != "idle" {
		t.Fatalf("a ready but unused magnet is in state %q, want idle", got)
	}
	if got := session.ConnectionAllowanceForTest(state); got != 0 {
		t.Fatalf("a ready but unused magnet has allowance %d, want 0", got)
	}
}

func totalActivePeers(sess *session.Session) int {
	total := 0
	for _, activity := range sess.TorrentPeerActivities() {
		total += activity.ActivePeers
	}
	return total
}
