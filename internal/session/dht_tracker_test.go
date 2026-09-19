package session_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	httptracker "github.com/anacrolix/torrent/tracker/http"
	"golang.org/x/time/rate"
)

func TestDHTQueryKeepsSocketAddressFamily(t *testing.T) {
	conn6, err := net.ListenPacket("udp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback is unavailable: %v", err)
	}
	t.Cleanup(func() { _ = conn6.Close() })

	conn4, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen IPv4 DHT socket: %v", err)
	}
	t.Cleanup(func() { _ = conn4.Close() })

	target, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen controlled IPv4 target: %v", err)
	}
	defer target.Close()
	targetAddr := dht.NewAddr(target.LocalAddr())

	server4 := newTestDHTServer(t, conn4)
	server6 := newTestDHTServer(t, conn6)

	query := dht.QueryInput{
		NumTries:     1,
		RateLimiting: dht.QueryRateLimiting{NoWaitFirst: true},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	ipv4Result := server4.Query(ctx, targetAddr, "ping", query)
	if ipv4Result.Writes != 1 {
		t.Fatalf("IPv4 DHT query writes = %d, want 1; err=%v", ipv4Result.Writes, ipv4Result.Err)
	}
	if ipv4Result.Err == nil {
		t.Fatal("IPv4 query unexpectedly received a response from the silent target")
	}

	ipv6Result := server6.Query(ctx, targetAddr, "ping", query)
	if ipv6Result.Writes != 0 {
		t.Skipf("udp6 accepted an IPv4 destination on this host: writes=%d err=%v", ipv6Result.Writes, ipv6Result.Err)
	}
	if ipv6Result.Err == nil {
		t.Fatal("IPv6 query to an IPv4 destination unexpectedly succeeded")
	}
}

func TestTorrentClientDHTServersFollowAddressFamilyFlags(t *testing.T) {
	probe, err := net.ListenPacket("udp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback is unavailable: %v", err)
	}
	probe.Close()

	tests := []struct {
		name     string
		disable4 bool
		disable6 bool
		want     map[string]bool
	}{
		{name: "dual stack", want: map[string]bool{"udp4": true, "udp6": true}},
		{name: "IPv4 only", disable6: true, want: map[string]bool{"udp4": true}},
		{name: "IPv6 only", disable4: true, want: map[string]bool{"udp6": true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := torrent.NewDefaultClientConfig()
			cfg.ListenHost = func(string) string { return "" }
			cfg.ListenPort = 0
			cfg.DataDir = t.TempDir()
			cfg.DisableTCP = true
			cfg.DisableUTP = true
			cfg.DisableIPv4 = tt.disable4
			cfg.DisableIPv6 = tt.disable6
			cfg.NoDefaultPortForwarding = true
			cfg.DhtStartingNodes = func(string) dht.StartingNodesGetter {
				return func() ([]dht.Addr, error) {
					return []dht.Addr{
						dht.NewAddr(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}),
					}, nil
				}
			}
			cfg.ConfigureAnacrolixDhtServer = func(server *dht.ServerConfig) {
				server.QueryResendDelay = func() time.Duration { return time.Millisecond }
			}

			client, err := torrent.NewClient(cfg)
			if err != nil {
				t.Fatalf("new torrent client: %v", err)
			}
			t.Cleanup(func() {
				if errs := client.Close(); len(errs) != 0 {
					t.Errorf("close torrent client: %v", errs)
				}
			})

			got := make(map[string]bool)
			for _, server := range client.DhtServers() {
				got[dhtServerFamily(server.Addr())] = true
			}
			if len(got) != len(tt.want) {
				t.Fatalf("DHT server families = %v, want %v", got, tt.want)
			}
			for network := range tt.want {
				if !got[network] {
					t.Errorf("DHT server families = %v, missing %s", got, network)
				}
			}
		})
	}
}

func TestHTTPTrackerEmptyAndNonEmptyPeersAreSuccessfulSnapshots(t *testing.T) {
	tests := []struct {
		name      string
		peers     string
		wantPeers int
	}{
		{name: "empty", wantPeers: 0},
		{name: "one peer", peers: compactPeerList([]string{"127.0.0.1:6881"}), wantPeers: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				body, err := bencodeTrackerResponse(tt.peers)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write(body)
			}))
			t.Cleanup(server.Close)

			trackerURL, err := url.Parse(server.URL + "/announce")
			if err != nil {
				t.Fatalf("parse tracker URL: %v", err)
			}
			client := httptracker.NewClient(trackerURL, httptracker.NewClientOpts{})
			t.Cleanup(func() { _ = client.Close() })

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			response, err := client.Announce(ctx, httptracker.AnnounceRequest{}, httptracker.AnnounceOpt{})
			if err != nil {
				t.Fatalf("announce: %v", err)
			}
			if response.Interval != 60 || response.Seeders != 0 || response.Leechers != 0 {
				t.Fatalf("response counters = interval %d, seeders %d, leechers %d; want 60/0/0", response.Interval, response.Seeders, response.Leechers)
			}
			if len(response.Peers) != tt.wantPeers {
				t.Fatalf("response peers = %d, want %d", len(response.Peers), tt.wantPeers)
			}
		})
	}
}

func dhtServerFamily(addr net.Addr) string {
	udpAddr, ok := addr.(*net.UDPAddr)
	if !ok {
		return addr.Network()
	}
	if udpAddr.IP.To4() != nil {
		return "udp4"
	}
	return "udp6"
}

func newTestDHTServer(t *testing.T, conn net.PacketConn) *dht.Server {
	t.Helper()
	server, err := dht.NewServer(&dht.ServerConfig{
		Conn:        conn,
		NoSecurity:  true,
		SendLimiter: rate.NewLimiter(rate.Inf, 1),
		QueryResendDelay: func() time.Duration {
			return time.Millisecond
		},
	})
	if err != nil {
		t.Fatalf("new DHT server: %v", err)
	}
	t.Cleanup(server.Close)
	return server
}

func bencodeTrackerResponse(peers string) ([]byte, error) {
	return bencode.Marshal(trackerAnnounceResponse{
		Interval:   60,
		Complete:   0,
		Incomplete: 0,
		Peers:      peers,
	})
}
