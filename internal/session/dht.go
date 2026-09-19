package session

import (
	"net"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/torrent"
)

// The upstream bootstrap resolver ignores the network argument, so filter after the socket is known.
func configureDhtStartingNodes(cc *torrent.ClientConfig) {
	existing := cc.ConfigureAnacrolixDhtServer
	cc.ConfigureAnacrolixDhtServer = func(server *dht.ServerConfig) {
		if existing != nil {
			existing(server)
		}
		if server.Conn == nil || server.StartingNodes == nil {
			return
		}

		network := dhtNetworkForConn(server.Conn)
		startingNodes := server.StartingNodes
		server.StartingNodes = func() ([]dht.Addr, error) {
			nodes, err := startingNodes()
			if err != nil {
				return nil, err
			}
			return filterDhtStartingNodes(network, nodes), nil
		}
	}
}

func dhtNetworkForConn(conn net.PacketConn) string {
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return conn.LocalAddr().Network()
	}
	if addr.IP.To4() != nil {
		return "udp4"
	}
	if addr.IP.To16() != nil {
		return "udp6"
	}
	return addr.Network()
}

func filterDhtStartingNodes(network string, nodes []dht.Addr) []dht.Addr {
	if network != "udp4" && network != "udp6" {
		return nodes
	}

	filtered := make([]dht.Addr, 0, len(nodes))
	for _, node := range nodes {
		ip := node.IP()
		if network == "udp4" && ip.To4() == nil {
			continue
		}
		if network == "udp6" && (ip.To4() != nil || ip.To16() == nil) {
			continue
		}
		filtered = append(filtered, node)
	}
	return filtered
}
