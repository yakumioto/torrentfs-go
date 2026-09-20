package session

import (
	"bytes"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/torrent"
)

func TestDHTNetworkForConnUsesSocketIPFamily(t *testing.T) {
	tests := []struct {
		name    string
		network string
		address string
		want    string
	}{
		{name: "IPv4", network: "udp4", address: "127.0.0.1:0", want: "udp4"},
		{name: "IPv6", network: "udp6", address: "[::1]:0", want: "udp6"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := net.ListenPacket(tt.network, tt.address)
			if err != nil {
				if tt.network == "udp6" {
					t.Skipf("IPv6 loopback is unavailable: %v", err)
				}
				t.Fatalf("listen %s: %v", tt.network, err)
			}
			defer func() {
				if err := conn.Close(); err != nil {
					t.Errorf("close %s socket: %v", tt.network, err)
				}
			}()

			if got := dhtNetworkForConn(conn); got != tt.want {
				t.Fatalf("dhtNetworkForConn = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFilterDHTStartingNodesBySocketFamily(t *testing.T) {
	nodes := []dht.Addr{
		dht.NewAddr(&net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 6881}),
		dht.NewAddr(&net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 6881}),
		dht.NewAddr(&net.UDPAddr{IP: net.ParseIP("::ffff:192.0.2.2"), Port: 6881}),
	}

	tests := []struct {
		name        string
		network     string
		wantAddress []string
	}{
		{name: "IPv4", network: "udp4", wantAddress: []string{"192.0.2.1:6881", "192.0.2.2:6881"}},
		{name: "IPv6", network: "udp6", wantAddress: []string{"[2001:db8::1]:6881"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterDhtStartingNodes(tt.network, nodes)
			if len(got) != len(tt.wantAddress) {
				t.Fatalf("filtered nodes = %d, want %d", len(got), len(tt.wantAddress))
			}
			for i, want := range tt.wantAddress {
				if got[i].String() != want {
					t.Errorf("filtered node %d = %q, want %q", i, got[i].String(), want)
				}
			}
		})
	}
}

// TestFilterDHTStartingNodesIPv4OnlyResolverMatchesLog reproduces the logged
// failure mode: DNS answers with IPv4 addresses only, so the udp6 socket is
// left with nothing while udp4 keeps every address.
func TestFilterDHTStartingNodesIPv4OnlyResolverMatchesLog(t *testing.T) {
	nodes := []dht.Addr{
		dht.NewAddr(&net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 6881}),
		dht.NewAddr(&net.UDPAddr{IP: net.IPv4(198, 51, 100, 7), Port: 6881}),
	}

	if got := filterDhtStartingNodes("udp4", nodes); len(got) != 2 {
		t.Fatalf("udp4 filtered nodes = %d, want 2", len(got))
	}
	if got := filterDhtStartingNodes("udp6", nodes); len(got) != 0 {
		t.Fatalf("udp6 filtered nodes = %d, want 0", len(got))
	}
}

// TestStaticStartingNodesServeEachFamily checks the explicit bootstrap path:
// one entry per family resolves without DNS and reaches the family that asked.
func TestStaticStartingNodesServeEachFamily(t *testing.T) {
	getter := staticStartingNodes([]string{"127.0.0.1:6881", "[::1]:6881"})

	nodes, err := getter()
	if err != nil {
		t.Fatalf("starting nodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("starting nodes = %v, want one address per family", nodes)
	}
	if got := filterDhtStartingNodes("udp4", nodes); len(got) != 1 || got[0].String() != "127.0.0.1:6881" {
		t.Fatalf("udp4 nodes = %v, want [127.0.0.1:6881]", got)
	}
	if got := filterDhtStartingNodes("udp6", nodes); len(got) != 1 || got[0].String() != "[::1]:6881" {
		t.Fatalf("udp6 nodes = %v, want [[::1]:6881]", got)
	}
}

// TestDHTRecorderWarnsOncePerStateChange pins the "bounded" part of Phase 1.2:
// a family that keeps failing the same way is reported once, and recovery is
// reported when it happens.
func TestDHTRecorderWarnsOncePerStateChange(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	recorder := newDhtRecorder()
	unavailable := dhtFamilyState{family: "udp6", localAddr: "[::1]:42000", resolved: 8, kept: 0}

	for range 3 {
		recorder.observe(logger, unavailable)
	}
	if got := strings.Count(buf.String(), "dht starting nodes unavailable"); got != 1 {
		t.Fatalf("unavailable warnings = %d, want 1; log=%q", got, buf.String())
	}
	if !strings.Contains(buf.String(), "dht_udp6_unavailable") {
		t.Fatalf("warning lost its status name: %q", buf.String())
	}

	recorder.observe(logger, dhtFamilyState{family: "udp6", localAddr: "[::1]:42000", resolved: 8, kept: 3})
	if got := strings.Count(buf.String(), "dht starting nodes available"); got != 1 {
		t.Fatalf("availability notices = %d, want 1; log=%q", got, buf.String())
	}

	snapshot := recorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("snapshot = %+v, want one family", snapshot)
	}
	if !snapshot[0].Ready || snapshot[0].Kept != 3 || snapshot[0].Resolved != 8 || snapshot[0].Family != "udp6" {
		t.Fatalf("snapshot = %+v, want a ready udp6 family", snapshot[0])
	}
}

// TestDHTRecorderStopsLoggingAfterStop pins the shutdown guarantee: a late DHT
// routing-table refresh must not reach the session's log sink once the session
// has closed. It is deterministic -- no goroutines, no timing.
func TestDHTRecorderStopsLoggingAfterStop(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	recorder := newDhtRecorder()

	recorder.observe(logger, dhtFamilyState{family: "udp6", localAddr: "[::1]:42000", resolved: 8})
	if got := strings.Count(buf.String(), "dht starting nodes unavailable"); got != 1 {
		t.Fatalf("warnings before stop = %d, want 1; log=%q", got, buf.String())
	}

	recorder.stop()
	recorder.observe(logger, dhtFamilyState{family: "udp4", localAddr: "127.0.0.1:42000", resolved: 8})
	if got := strings.Count(buf.String(), "dht starting nodes unavailable"); got != 1 {
		t.Fatalf("warnings after stop = %d, want 1; log=%q", got, buf.String())
	}
}

func TestConfigureDHTStartingNodesFiltersEachSocketFamily(t *testing.T) {
	nodes := []dht.Addr{
		dht.NewAddr(&net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 6881}),
		dht.NewAddr(&net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 6881}),
	}

	cc := torrent.NewDefaultClientConfig()
	cc.DhtStartingNodes = func(string) dht.StartingNodesGetter {
		return func() ([]dht.Addr, error) {
			return nodes, nil
		}
	}
	configureDhtStartingNodes(cc, nil, nil)

	tests := []struct {
		name    string
		network string
		address string
		want    string
	}{
		{name: "IPv4", network: "udp4", address: "127.0.0.1:0", want: "192.0.2.1:6881"},
		{name: "IPv6", network: "udp6", address: "[::1]:0", want: "[2001:db8::1]:6881"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := net.ListenPacket(tt.network, tt.address)
			if err != nil {
				if tt.network == "udp6" {
					t.Skipf("IPv6 loopback is unavailable: %v", err)
				}
				t.Fatalf("listen %s: %v", tt.network, err)
			}
			defer func() {
				if err := conn.Close(); err != nil {
					t.Errorf("close %s socket: %v", tt.network, err)
				}
			}()

			server := &dht.ServerConfig{
				Conn:          conn,
				StartingNodes: cc.DhtStartingNodes("udp"),
			}
			cc.ConfigureAnacrolixDhtServer(server)
			got, err := server.StartingNodes()
			if err != nil {
				t.Fatalf("starting nodes: %v", err)
			}
			if len(got) != 1 || got[0].String() != tt.want {
				t.Fatalf("starting nodes = %v, want [%s]", got, tt.want)
			}
		})
	}
}
