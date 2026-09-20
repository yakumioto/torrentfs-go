package session_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	pp "github.com/anacrolix/torrent/peer_protocol"
	"github.com/anacrolix/torrent/types/infohash"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

type clientIdentity struct {
	trackerUserAgent               string
	peerIDPrefix                   string
	extendedHandshakeClientVersion string
	peerID                         string
}

type handshakeEvents struct {
	peerIDs  chan []byte
	versions chan string
}

func newHandshakeEvents() *handshakeEvents {
	return &handshakeEvents{
		peerIDs:  make(chan []byte, 32),
		versions: make(chan string, 32),
	}
}

func (e *handshakeEvents) configure(cc *session.TorrentClientConfig) {
	cc.Callbacks.CompletedHandshake = func(pc *torrent.PeerConn, _ infohash.T) {
		peerID := append([]byte(nil), pc.PeerID[:]...)
		select {
		case e.peerIDs <- peerID:
		default:
		}
	}
	cc.Callbacks.ReadExtendedHandshake = func(_ *torrent.PeerConn, msg *pp.ExtendedHandshakeMessage) {
		select {
		case e.versions <- msg.V:
		default:
		}
	}
}

func identityTorrentsDir(t *testing.T, base string) string {
	t.Helper()
	dir := filepath.Join(base, "torrents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	return dir
}

func newIdentitySession(t *testing.T, cfg config.Config, torrentsDir string, events *handshakeEvents) *session.Session {
	t.Helper()
	cfg.Connections.ListenHost = "127.0.0.1"
	sess, err := session.NewWithClientConfig(cfg, torrentsDir, func(cc *session.TorrentClientConfig) {
		cc.NoDHT = true
		cc.DisableUTP = true
		cc.DisableIPv6 = true
		cc.NoDefaultPortForwarding = true
		cc.AlwaysWantConns = true
		if events != nil {
			events.configure(cc)
		}
	})
	if err != nil {
		t.Fatalf("NewWithClientConfig: %v", err)
	}
	t.Cleanup(func() {
		if err := sess.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return sess
}

func TestIdentityMapsToClientConfig(t *testing.T) {
	t.Run("defaults use qBittorrent identity", func(t *testing.T) {
		var got clientIdentity
		cfg := testConfig()
		sess, err := session.NewWithClientConfig(cfg, identityTorrentsDir(t, t.TempDir()), func(cc *session.TorrentClientConfig) {
			got = clientIdentity{
				trackerUserAgent:               cc.HTTPUserAgent,
				peerIDPrefix:                   cc.Bep20,
				extendedHandshakeClientVersion: cc.ExtendedHandshakeClientVersion,
				peerID:                         cc.PeerID,
			}
			cc.NoDHT = true
			cc.DisableUTP = true
			cc.DisableIPv6 = true
			cc.NoDefaultPortForwarding = true
		})
		if err != nil {
			t.Fatalf("NewWithClientConfig: %v", err)
		}
		defer func() {
			if err := sess.Close(context.Background()); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()

		want := clientIdentity{
			trackerUserAgent:               "qBittorrent/4.4.0",
			peerIDPrefix:                   "-qB4400-",
			extendedHandshakeClientVersion: "qBittorrent/4.4.0",
			peerID:                         "",
		}
		if got != want {
			t.Fatalf("client identity = %+v, want %+v", got, want)
		}
	})

	tests := []struct {
		name       string
		identity   config.Identity
		wantPrefix string
	}{
		{
			name: "qBittorrent 4.4.0",
			identity: config.Identity{
				TrackerUserAgent:               "qBittorrent/4.4.0",
				PeerIDPrefix:                   "-qB4400-",
				ExtendedHandshakeClientVersion: "qBittorrent/4.4.0",
			},
			wantPrefix: "-qB4400-",
		},
		{
			name: "Transmission 3.00",
			identity: config.Identity{
				TrackerUserAgent:               "Transmission/3.00",
				PeerIDPrefix:                   "-TR3000-",
				ExtendedHandshakeClientVersion: "Transmission/3.00",
			},
			wantPrefix: "-TR3000-",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Identity = tt.identity
			var got clientIdentity
			sess, err := session.NewWithClientConfig(cfg, identityTorrentsDir(t, t.TempDir()), func(cc *session.TorrentClientConfig) {
				got = clientIdentity{
					trackerUserAgent:               cc.HTTPUserAgent,
					peerIDPrefix:                   cc.Bep20,
					extendedHandshakeClientVersion: cc.ExtendedHandshakeClientVersion,
					peerID:                         cc.PeerID,
				}
				cc.NoDHT = true
				cc.DisableUTP = true
				cc.DisableIPv6 = true
				cc.NoDefaultPortForwarding = true
			})
			if err != nil {
				t.Fatalf("NewWithClientConfig: %v", err)
			}
			defer func() {
				if err := sess.Close(context.Background()); err != nil {
					t.Errorf("Close: %v", err)
				}
			}()

			want := clientIdentity{
				trackerUserAgent:               tt.identity.TrackerUserAgent,
				peerIDPrefix:                   tt.wantPrefix,
				extendedHandshakeClientVersion: tt.identity.ExtendedHandshakeClientVersion,
				peerID:                         "",
			}
			if got != want {
				t.Fatalf("client identity = %+v, want %+v", got, want)
			}
		})
	}
}

func TestIdentityReachesTrackerAndPeerHandshake(t *testing.T) {
	tracker := newLoopbackTracker(t)
	work := t.TempDir()
	content := []byte("client identity handshake payload")
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "payload.bin", content, [][]string{{tracker.url}})
	torrentPath := filepath.Join(work, "identity.torrent")
	if err := os.WriteFile(torrentPath, torrentBytes, 0o644); err != nil {
		t.Fatalf("write torrent: %v", err)
	}

	transmissionIdentity := config.Identity{
		TrackerUserAgent:               "Transmission/3.00",
		PeerIDPrefix:                   "-TR3000-",
		ExtendedHandshakeClientVersion: "Transmission/3.00",
	}
	qbEvents := newHandshakeEvents()
	transmissionEvents := newHandshakeEvents()

	qbTorrentsDir := identityTorrentsDir(t, filepath.Join(work, "qbittorrent-data"))
	transmissionTorrentsDir := identityTorrentsDir(t, filepath.Join(work, "transmission-data"))
	qbConfig := testConfig()
	transmissionConfig := testConfig()
	transmissionConfig.Identity = transmissionIdentity
	qb := newIdentitySession(t, qbConfig, qbTorrentsDir, qbEvents)
	transmission := newIdentitySession(t, transmissionConfig, transmissionTorrentsDir, transmissionEvents)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := qb.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("qBittorrent AddTorrent: %v", err)
	}
	seedPieces(t, qb, hash, content)
	if err := transmission.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("Transmission AddTorrent: %v", err)
	}

	hashHex := hash.HexString()
	if err := waitFor(ctx, func() bool { return tracker.announceCount(hashHex) >= 2 }); err != nil {
		t.Fatalf("tracker never saw both peers announce: %v", err)
	}
	assertTrackerIdentities(t, tracker.announceSnapshot(hashHex))

	if err := waitForPeerIDPrefix(ctx, transmissionEvents.peerIDs, "-qB4400-"); err != nil {
		t.Fatalf("Transmission did not observe qBittorrent peer ID: %v", err)
	}
	if err := waitForVersion(ctx, transmissionEvents.versions, "qBittorrent/4.4.0"); err != nil {
		t.Fatalf("Transmission did not observe qBittorrent version: %v", err)
	}
	if err := waitForPeerIDPrefix(ctx, qbEvents.peerIDs, "-TR3000-"); err != nil {
		t.Fatalf("qBittorrent did not observe Transmission peer ID: %v", err)
	}
	if err := waitForVersion(ctx, qbEvents.versions, "Transmission/3.00"); err != nil {
		t.Fatalf("qBittorrent did not observe Transmission version: %v", err)
	}
}

func assertTrackerIdentities(t *testing.T, announces []trackerAnnounce) {
	t.Helper()
	seen := make(map[string]bool)
	for _, announce := range announces {
		var prefix string
		switch announce.UserAgent {
		case "qBittorrent/4.4.0":
			prefix = "-qB4400-"
		case "Transmission/3.00":
			prefix = "-TR3000-"
		default:
			continue
		}
		if len(announce.PeerID) != 20 {
			t.Errorf("tracker peer ID for %q has length %d, want 20", announce.UserAgent, len(announce.PeerID))
			continue
		}
		if !bytes.HasPrefix(announce.PeerID, []byte(prefix)) {
			t.Errorf("tracker peer ID for %q = %q, want prefix %q", announce.UserAgent, announce.PeerID, prefix)
			continue
		}
		seen[announce.UserAgent] = true
	}
	for _, userAgent := range []string{"qBittorrent/4.4.0", "Transmission/3.00"} {
		if !seen[userAgent] {
			t.Errorf("tracker did not record a valid announce for %q", userAgent)
		}
	}
}

func waitForPeerIDPrefix(ctx context.Context, events <-chan []byte, prefix string) error {
	for {
		select {
		case peerID := <-events:
			if len(peerID) == 20 && bytes.HasPrefix(peerID, []byte(prefix)) {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func waitForVersion(ctx context.Context, events <-chan string, version string) error {
	for {
		select {
		case got := <-events:
			if got == version {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
