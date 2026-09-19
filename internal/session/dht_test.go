package session

import (
	"net"
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
	configureDhtStartingNodes(cc)

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
