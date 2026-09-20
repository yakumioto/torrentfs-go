package session_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/dht/v2"

	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/session"
)

// newLivingSession creates a session on loopback that never touches DHT and
// returns it without auto-closing: restart and lock tests need explicit control
// over the session lifetime.
func newLivingSession(t *testing.T, cfg config.Config, customize func(*session.TorrentClientConfig)) *session.Session {
	t.Helper()
	cfg.Connections.ListenHost = "127.0.0.1"
	torrentsDir := filepath.Join(cfg.Paths.DataDir, "torrents")
	if err := os.MkdirAll(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	sess, err := session.NewWithClientConfig(cfg, torrentsDir, func(cc *session.TorrentClientConfig) {
		cc.NoDHT = true
		cc.DisableUTP = true
		cc.DisableIPv6 = true
		cc.NoDefaultPortForwarding = true
		if customize != nil {
			customize(cc)
		}
	})
	if err != nil {
		t.Fatalf("NewWithClientConfig: %v", err)
	}
	return sess
}

func closeSession(t *testing.T, sess *session.Session) {
	t.Helper()
	if err := sess.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestConnectionSettingsMapToClientConfig covers Phase 2.1 and 2.2: the new
// connection keys reach the client, and no_port_forwarding is already true
// under the built-in defaults.
func TestConnectionSettingsMapToClientConfig(t *testing.T) {
	tests := []struct {
		name                      string
		configure                 func(*config.Config)
		wantDisableIPv4           bool
		wantDisableIPv6           bool
		wantPortForwardingEnabled bool
	}{
		{
			name:                      "defaults disable port forwarding",
			wantPortForwardingEnabled: false,
		},
		{
			name:            "disable_ipv6",
			configure:       func(cfg *config.Config) { cfg.Connections.DisableIPv6 = true },
			wantDisableIPv6: true,
		},
		{
			name:            "disable_ipv4",
			configure:       func(cfg *config.Config) { cfg.Connections.DisableIPv4 = true },
			wantDisableIPv4: true,
		},
		{
			name:                      "port forwarding opt-in",
			configure:                 func(cfg *config.Config) { cfg.Connections.NoPortForwarding = false },
			wantPortForwardingEnabled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(t.TempDir())
			if tt.configure != nil {
				tt.configure(&cfg)
			}
			cfg.Connections.ListenHost = "127.0.0.1"
			torrentsDir := filepath.Join(cfg.Paths.DataDir, "torrents")
			if err := os.MkdirAll(torrentsDir, 0o755); err != nil {
				t.Fatalf("make torrents dir: %v", err)
			}
			var got struct {
				disableIPv4    bool
				disableIPv6    bool
				portForwarding bool
			}
			// The customize seam must only read: anything it wrote would
			// override the production mapping this test asserts.
			sess, err := session.NewWithClientConfig(cfg, torrentsDir, func(cc *session.TorrentClientConfig) {
				got.disableIPv4 = cc.DisableIPv4
				got.disableIPv6 = cc.DisableIPv6
				got.portForwarding = !cc.NoDefaultPortForwarding
				// The assertions are about mapped values, not sockets: skip
				// listening so the test does not depend on which families this
				// host can bind.
				cc.NoDHT = true
				cc.DisableTCP = true
				cc.DisableUTP = true
			})
			if err != nil {
				t.Fatalf("NewWithClientConfig: %v", err)
			}
			defer closeSession(t, sess)

			if got.disableIPv4 != tt.wantDisableIPv4 || got.disableIPv6 != tt.wantDisableIPv6 {
				t.Fatalf("address family flags = ipv4 %t ipv6 %t, want %t/%t", got.disableIPv4, got.disableIPv6, tt.wantDisableIPv4, tt.wantDisableIPv6)
			}
			if got.portForwarding != tt.wantPortForwardingEnabled {
				t.Fatalf("port forwarding enabled = %t, want %t", got.portForwarding, tt.wantPortForwardingEnabled)
			}
		})
	}
}

// TestDisableIPv6KeepsOnlyIPv4DHTServers checks that the connection flag, not
// just the raw client option, decides how many DHT servers run.
func TestDisableIPv6KeepsOnlyIPv4DHTServers(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.Connections.DisableIPv6 = true
	cfg.Connections.ListenHost = "127.0.0.1"
	torrentsDir := filepath.Join(cfg.Paths.DataDir, "torrents")
	if err := os.MkdirAll(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}

	sess, err := session.NewWithClientConfig(cfg, torrentsDir, func(cc *session.TorrentClientConfig) {
		cc.DisableUTP = true
		cc.NoDefaultPortForwarding = true
		cc.DhtStartingNodes = loopbackStartingNodes
	})
	if err != nil {
		t.Fatalf("NewWithClientConfig: %v", err)
	}
	defer closeSession(t, sess)

	families := sess.DhtServerFamiliesForTest()
	if len(families) != 1 || families[0] != "udp4" {
		t.Fatalf("DHT server families = %v, want [udp4]", families)
	}
}

// TestDisableIPv4KeepsOnlyIPv6DHTServers is the mirror case. IPv6 loopback is
// not universally available, so it skips rather than fails there.
func TestDisableIPv4KeepsOnlyIPv6DHTServers(t *testing.T) {
	probe, err := net.ListenPacket("udp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback is unavailable: %v", err)
	}
	if err := probe.Close(); err != nil {
		t.Fatalf("close IPv6 probe: %v", err)
	}

	cfg := testConfig(t.TempDir())
	cfg.Connections.DisableIPv4 = true
	cfg.Connections.ListenHost = "::1"
	torrentsDir := filepath.Join(cfg.Paths.DataDir, "torrents")
	if err := os.MkdirAll(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}

	sess, err := session.NewWithClientConfig(cfg, torrentsDir, func(cc *session.TorrentClientConfig) {
		cc.DisableUTP = true
		cc.NoDefaultPortForwarding = true
		cc.DhtStartingNodes = loopbackStartingNodes
	})
	if err != nil {
		t.Fatalf("NewWithClientConfig: %v", err)
	}
	defer closeSession(t, sess)

	families := sess.DhtServerFamiliesForTest()
	if len(families) != 1 || families[0] != "udp6" {
		t.Fatalf("DHT server families = %v, want [udp6]", families)
	}
}

// loopbackStartingNodes keeps DHT bootstrap traffic on the loopback interface
// so the tests above never resolve or contact a public bootstrap host.
func loopbackStartingNodes(string) dht.StartingNodesGetter {
	return func() ([]dht.Addr, error) {
		return []dht.Addr{dht.NewAddr(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9})}, nil
	}
}

// TestEffectiveListenPortMatchesTrackerAnnounce pins Phase 1.1: with a dynamic
// configured port, the port reported to the operator and the port announced to
// the tracker are the same real value.
func TestEffectiveListenPortMatchesTrackerAnnounce(t *testing.T) {
	tracker := newLoopbackTracker(t)
	work := t.TempDir()
	content := []byte("effective listen port announce payload")
	torrentBytes, hash := buildSingleFileTorrentBytes(t, "port.bin", content, [][]string{{tracker.url}})
	torrentPath := filepath.Join(work, "port.torrent")
	if err := os.WriteFile(torrentPath, torrentBytes, 0o644); err != nil {
		t.Fatalf("write torrent: %v", err)
	}

	cfg := testConfig(filepath.Join(work, "data"))
	cfg.Connections.ListenPort = 0
	sess := newLivingSession(t, cfg, nil)
	defer closeSession(t, sess)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := sess.AddTorrent(ctx, session.Source{MetainfoPath: torrentPath}); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}

	hashHex := hash.HexString()
	if err := waitFor(ctx, func() bool { return tracker.announceCount(hashHex) >= 1 }); err != nil {
		t.Fatalf("tracker never saw an announce: %v", err)
	}

	effective := sess.EffectiveListenPort()
	if effective == 0 {
		t.Fatal("effective listen port is 0 with a dynamic configured port")
	}
	announces := tracker.announceSnapshot(hashHex)
	if announces[0].Port != effective {
		t.Fatalf("tracker announce port = %d, effective listen port = %d", announces[0].Port, effective)
	}
	if announces[0].Event != "started" {
		t.Fatalf("first announce event = %q, want started", announces[0].Event)
	}
}

// TestPeerIDPersistsAcrossRestarts pins Phase 3.1: a private tracker sees one
// stable peer identity instead of a new one per restart.
func TestPeerIDPersistsAcrossRestarts(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := testConfig(dataDir)

	first := newLivingSession(t, cfg, nil)
	firstID := first.PeerIDForTest()
	closeSession(t, first)

	if len(firstID) != 20 {
		t.Fatalf("peer ID length = %d, want 20", len(firstID))
	}
	if !strings.HasPrefix(firstID, cfg.Identity.PeerIDPrefix) {
		t.Fatalf("peer ID %q does not carry prefix %q", firstID, cfg.Identity.PeerIDPrefix)
	}

	second := newLivingSession(t, cfg, nil)
	secondID := second.PeerIDForTest()
	closeSession(t, second)

	if secondID != firstID {
		t.Fatalf("peer ID changed across restart: %q -> %q", firstID, secondID)
	}
	stored, err := os.ReadFile(filepath.Join(dataDir, "peer_id"))
	if err != nil {
		t.Fatalf("read peer ID file: %v", err)
	}
	if string(stored) != firstID {
		t.Fatalf("stored peer ID = %q, want %q", stored, firstID)
	}
}

func TestPeerIDDiffersPerDataDir(t *testing.T) {
	first := newLivingSession(t, testConfig(filepath.Join(t.TempDir(), "data")), nil)
	firstID := first.PeerIDForTest()
	closeSession(t, first)

	second := newLivingSession(t, testConfig(filepath.Join(t.TempDir(), "data")), nil)
	secondID := second.PeerIDForTest()
	closeSession(t, second)

	if firstID == secondID {
		t.Fatalf("two data directories produced the same peer ID %q", firstID)
	}
}

// TestCorruptPeerIDFileFailsStartup keeps the failure loud: a damaged identity
// file must not silently produce a new identity, which is the drift Phase 3.1
// exists to stop.
func TestCorruptPeerIDFileFailsStartup(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("make data dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "peer_id"), []byte("too-short"), 0o644); err != nil {
		t.Fatalf("write corrupt peer ID: %v", err)
	}

	cfg := testConfig(dataDir)
	torrentsDir := filepath.Join(dataDir, "torrents")
	if err := os.MkdirAll(torrentsDir, 0o755); err != nil {
		t.Fatalf("make torrents dir: %v", err)
	}
	sess, err := session.NewWithClientConfig(cfg, torrentsDir, func(cc *session.TorrentClientConfig) {
		cc.NoDHT = true
		cc.DisableUTP = true
		cc.NoDefaultPortForwarding = true
	})
	if err == nil {
		closeSession(t, sess)
		t.Fatal("session started with a corrupt peer ID file")
	}
	if !strings.Contains(err.Error(), "peer ID file") {
		t.Fatalf("startup error = %v, want a peer ID file error", err)
	}
}

// TestSecondInstanceOnSameTorrentsDirFails pins Phase 3.2: one torrents
// directory is managed by one process, and the lock is released on close.
func TestSecondInstanceOnSameTorrentsDirFails(t *testing.T) {
	cfg := testConfig(filepath.Join(t.TempDir(), "data"))
	torrentsDir := filepath.Join(cfg.Paths.DataDir, "torrents")

	first := newLivingSession(t, cfg, nil)

	_, err := session.NewWithClientConfig(cfg, torrentsDir, func(cc *session.TorrentClientConfig) {
		cc.NoDHT = true
		cc.DisableUTP = true
		cc.NoDefaultPortForwarding = true
	})
	if err == nil {
		t.Fatal("a second session opened the same torrents directory")
	}
	if !strings.Contains(err.Error(), "another torrentfs instance") {
		t.Fatalf("second instance error = %v, want an instance-lock error", err)
	}

	closeSession(t, first)

	second := newLivingSession(t, cfg, nil)
	closeSession(t, second)
}
