package session_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	pp "github.com/anacrolix/torrent/peer_protocol"

	"github.com/yakumioto/torrentfs-go/internal/session"
)

// These tests pin BEP 27 isolation: a torrent whose metainfo carries
// private=1 must not be announced to or discovered via DHT, and must not
// exchange peers over PEX (ut_pex). Both assertions come in pairs: a public
// torrent in the same session is the positive control, proving the discovery
// path actually runs under the test's conditions. Without the control a
// silent private torrent could pass simply because nothing ever fired.
//
// v1.61.0 has no Local Peer Discovery (BEP 14) support at all, so there is
// nothing to assert for LPD here. If a future dependency bump enables upstream
// LPD, the private-torrent guard must be re-verified for that path too.
//
// The announce counter is read through Client.WriteStatus, which is only a
// usable observation point while every accessor it reaches is synchronized.
// It reads the tracker dispatcher's timer deadline via mytimer.Timer.When(),
// which upstream leaves unlocked. v1.61.0-bep27.2 (MIO-46) adds a read lock
// there; the current v1.61.0-bep27.3 pin retains that fix and adds MIO-62's
// closed peer-request shutdown cleanup. Before any future dependency bump,
// re-check that WriteStatus is race-free with the whole package running.

const bep27SettleWindow = 3 * time.Second

// buildSingleFileTorrentBytesWithPrivacy encodes a single-file .torrent like
// buildSingleFileTorrentBytes, additionally setting BEP 27's private flag when
// private is true. The flag changes the info dict, so the returned info hash
// differs from the public build.
func buildSingleFileTorrentBytesWithPrivacy(t *testing.T, name string, data []byte, private bool) ([]byte, metainfo.Hash) {
	t.Helper()
	pieces := make([]byte, 0, (len(data)+testPieceLength-1)/testPieceLength*sha1.Size)
	for off := 0; off < len(data); off += testPieceLength {
		end := off + testPieceLength
		if end > len(data) {
			end = len(data)
		}
		sum := sha1.Sum(data[off:end])
		pieces = append(pieces, sum[:]...)
	}
	info := metainfo.Info{
		Name:        name,
		Length:      int64(len(data)),
		PieceLength: testPieceLength,
		Pieces:      pieces,
	}
	if private {
		priv := true
		info.Private = &priv
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatalf("encode info: %v", err)
	}
	mi := metainfo.MetaInfo{InfoBytes: bencode.Bytes(infoBytes)}
	torrentBytes, err := bencode.Marshal(mi)
	if err != nil {
		t.Fatalf("encode metainfo: %v", err)
	}
	return torrentBytes, mi.HashInfoBytes()
}

func addMetainfoBytes(t *testing.T, ctx context.Context, sess *session.Session, torrentBytes []byte) {
	t.Helper()
	if err := sess.AddTorrent(ctx, session.Source{Metainfo: torrentBytes}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
}

func requireTorrent(t *testing.T, sess *session.Session, hash metainfo.Hash) *session.Torrent {
	t.Helper()
	st, ok := sess.Torrent(hash)
	if !ok {
		t.Fatalf("session did not register torrent %s", hash.HexString())
	}
	return st
}

// dhtAnnouncesByTorrent reads the client status and maps each named torrent to
// its "DHT Announces" counter. The counter increments once per DHT announcer
// loop iteration, before any DHT I/O, so it is observable with an unreachable
// DHT.
func dhtAnnouncesByTorrent(t *testing.T, sess *session.Session, names ...string) map[string]int {
	t.Helper()
	var buf bytes.Buffer
	session.UnderlyingClientForTest(sess).WriteStatus(&buf)

	want := make(map[string]struct{}, len(names))
	for _, name := range names {
		want[name] = struct{}{}
	}
	out := make(map[string]int, len(names))
	current := ""
	for _, line := range strings.Split(buf.String(), "\n") {
		if _, ok := want[line]; ok {
			current = line
			continue
		}
		rest, ok := strings.CutPrefix(line, "DHT Announces: ")
		if !ok || current == "" {
			continue
		}
		count, err := strconv.Atoi(strings.TrimSpace(rest))
		if err != nil {
			t.Fatalf("parse DHT Announces line %q: %v", line, err)
		}
		out[current] = count
	}
	for _, name := range names {
		if _, ok := out[name]; !ok {
			t.Fatalf("client status has no DHT Announces line for torrent %q", name)
		}
	}
	return out
}

// TestBEP27PrivateTorrentDoesNotAnnounceToDHT keeps DHT enabled for the session
// but points bootstrap at a loopback black hole, so no reachable DHT node is
// needed. Both torrents are complete seeders, so both reach the announcer's
// eligibility check; only the private one must be held back by the BEP 27
// guard.
func TestBEP27PrivateTorrentDoesNotAnnounceToDHT(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	work := t.TempDir()
	torrentsDir := filepath.Join(work, "torrents")
	sess := newLoopbackSessionWithCustomize(t, testConfig(), torrentsDir,
		func(cc *session.TorrentClientConfig) {
			// DHT stays on; only the bootstrap target is replaced with a
			// loopback address nothing listens on.
			cc.NoDHT = false
			cc.DhtStartingNodes = func(string) dht.StartingNodesGetter {
				return func() ([]dht.Addr, error) {
					return []dht.Addr{dht.NewAddr(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1})}, nil
				}
			}
		})

	content := make([]byte, 2*testPieceLength)
	for i := range content {
		content[i] = byte(i%251 + 1)
	}
	publicBytes, publicHash := buildSingleFileTorrentBytesWithPrivacy(t, "public.bin", content, false)
	privateBytes, privateHash := buildSingleFileTorrentBytesWithPrivacy(t, "private.bin", content, true)
	if publicHash == privateHash {
		t.Fatal("private flag did not change the info hash")
	}
	addMetainfoBytes(t, ctx, sess, publicBytes)
	addMetainfoBytes(t, ctx, sess, privateBytes)

	// A seeding torrent only wants connections once it has a piece, and the
	// DHT announcer waits on the same condition.
	seedPieces(t, sess, publicHash, content)
	seedPieces(t, sess, privateHash, content)

	// Positive control: the public torrent must be observed announcing. If this
	// never happens the test is inconclusive, not passing.
	if err := waitFor(ctx, func() bool {
		return dhtAnnouncesByTorrent(t, sess, "public.bin", "private.bin")["public.bin"] >= 1
	}); err != nil {
		t.Fatalf("public torrent never announced to DHT, so the private assertion would be vacuous: %v", err)
	}

	// Negative: the private torrent must stay at zero across the settle window,
	// observed after the public control has already announced.
	deadline := time.Now().Add(bep27SettleWindow)
	for time.Now().Before(deadline) {
		if got := dhtAnnouncesByTorrent(t, sess, "public.bin", "private.bin")["private.bin"]; got != 0 {
			t.Fatalf("private torrent announced to DHT %d times, want 0", got)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestBEP27PrivateTorrentDoesNotExchangePEX runs a two-client loopback swarm and
// watches the seeder for incoming ut_pex extension messages, attributing each to
// the infohash it arrived for. The public torrent is the positive control.
func TestBEP27PrivateTorrentDoesNotExchangePEX(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	work := t.TempDir()
	seederDir := filepath.Join(work, "seeder-torrents")
	leecherDir := filepath.Join(work, "leecher-torrents")
	content := make([]byte, 2*testPieceLength)
	for i := range content {
		content[i] = byte(i%251 + 1)
	}
	publicBytes, publicHash := buildSingleFileTorrentBytesWithPrivacy(t, "public.bin", content, false)
	privateBytes, privateHash := buildSingleFileTorrentBytesWithPrivacy(t, "private.bin", content, true)

	var mu sync.Mutex
	pexByHash := map[metainfo.Hash]int{}
	connsByHash := map[metainfo.Hash]int{}
	seeder := newLoopbackSessionWithCustomize(t, testConfig(), seederDir,
		func(cc *session.TorrentClientConfig) {
			// Count established connections per infohash, so the private
			// negative can show that a private peer connection did exist and
			// still produced no PEX.
			cc.Callbacks.PeerConnAdded = append(cc.Callbacks.PeerConnAdded,
				func(c *torrent.PeerConn) {
					if c == nil || c.Torrent() == nil {
						return
					}
					hash := c.Torrent().InfoHash()
					mu.Lock()
					connsByHash[hash]++
					mu.Unlock()
				})
			cc.Callbacks.PeerConnReadExtensionMessage = append(
				cc.Callbacks.PeerConnReadExtensionMessage,
				func(e torrent.PeerConnReadExtensionMessageEvent) {
					if e.PeerConn == nil || e.PeerConn.LocalLtepProtocolMap == nil {
						return
					}
					name, _, err := e.PeerConn.LocalLtepProtocolMap.LookupId(e.ExtensionNumber)
					if err != nil || name != pp.ExtensionNamePex {
						return
					}
					remote := e.PeerConn.Torrent()
					if remote == nil {
						return
					}
					hash := remote.InfoHash()
					mu.Lock()
					pexByHash[hash]++
					mu.Unlock()
				})
		})

	addMetainfoBytes(t, ctx, seeder, publicBytes)
	addMetainfoBytes(t, ctx, seeder, privateBytes)
	// The seeder holds the data, so its connections complete as a real swarm
	// would rather than stalling on a download.
	seedPieces(t, seeder, publicHash, content)
	seedPieces(t, seeder, privateHash, content)

	leecher := newLoopbackSession(t, testConfig(), leecherDir)
	addMetainfoBytes(t, ctx, leecher, publicBytes)
	addMetainfoBytes(t, ctx, leecher, privateBytes)

	seederAddr := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(seeder.EffectiveListenPort()))
	for _, hash := range []metainfo.Hash{publicHash, privateHash} {
		st := requireTorrent(t, leecher, hash)
		// A torrent only dials out once it wants data; mark both for download
		// so the private swarm really connects rather than sitting idle.
		session.UnderlyingTorrentForTest(st).DownloadAll()
		session.UnderlyingTorrentForTest(st).AddPeers([]torrent.PeerInfo{{Addr: seederAddr}})
	}

	pexCount := func(hash metainfo.Hash) int {
		mu.Lock()
		defer mu.Unlock()
		return pexByHash[hash]
	}
	connCount := func(hash metainfo.Hash) int {
		mu.Lock()
		defer mu.Unlock()
		return connsByHash[hash]
	}

	// Positive control: the public infohash must exchange PEX over an
	// established connection.
	if err := waitFor(ctx, func() bool { return pexCount(publicHash) >= 1 }); err != nil {
		t.Fatalf("public infohash exchanged no PEX, so the private assertion would be vacuous: %v", err)
	}

	// The private swarm must have connected too, otherwise "no PEX" could just
	// mean "no peer". This is the control that makes the negative meaningful.
	if err := waitFor(ctx, func() bool { return connCount(privateHash) >= 1 }); err != nil {
		t.Fatalf("private infohash never established a connection, so its PEX assertion would be vacuous: %v", err)
	}

	// Negative: with a private connection established, PEX must still never be
	// exchanged for that infohash.
	deadline := time.Now().Add(bep27SettleWindow)
	for time.Now().Before(deadline) {
		if got := pexCount(privateHash); got != 0 {
			t.Fatalf("private infohash received %d PEX messages over %d connections, want 0",
				got, connCount(privateHash))
		}
		time.Sleep(100 * time.Millisecond)
	}
}
