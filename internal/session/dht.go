package session

import (
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"sync"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/torrent"
)

// DhtFamilyStatus describes one UDP address family's DHT view at snapshot time.
type DhtFamilyStatus struct {
	Family    string
	LocalAddr string
	// Resolved and Kept are the sizes of the bootstrap list before and after
	// address-family filtering on the last attempt. Kept is zero when the
	// resolver returned no address of this family, which is the state the
	// tracker-empty logs could not previously express.
	Resolved int
	Kept     int
	// Nodes and GoodNodes come from the live DHT server of this family. Both
	// stay zero when no server exists for the family.
	Nodes     int
	GoodNodes int
	// Ready reports whether the last bootstrap attempt kept at least one
	// starting node for this family.
	Ready bool
	Error string
}

// dhtFamilyState is one recorded bootstrap outcome.
type dhtFamilyState struct {
	family    string
	localAddr string
	resolved  int
	kept      int
	err       string
}

// dhtRecorder remembers the last starting-node outcome per address family so
// the session can report discovery health, and warn once per state change
// instead of once per table refresh. A family that keeps failing the same way
// stays quiet after its first warning.
type dhtRecorder struct {
	mu       sync.Mutex
	families map[string]dhtFamilyState
	// stopped is set by stop during session teardown. Once set, no further
	// record is logged. Because stop takes the same mu, a record already in
	// flight finishes logging before stop returns, so "logged by the DHT
	// goroutine" happens-before "stop returned" happens-before "Session.Close
	// returned". A caller that closes or reads the log sink after Close
	// therefore no longer races a late DHT record.
	stopped bool
}

func newDhtRecorder() *dhtRecorder {
	return &dhtRecorder{families: make(map[string]dhtFamilyState)}
}

func (r *dhtRecorder) observe(logger *slog.Logger, state dhtFamilyState) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	previous, seen := r.families[state.family]
	r.families[state.family] = state
	if logger == nil {
		return
	}

	switch {
	case state.err == "" && state.kept > 0:
		if seen && !(previous.err == "" && previous.kept > 0) {
			logger.Info("dht starting nodes available",
				"family", state.family,
				"local_addr", state.localAddr,
				"resolved", state.resolved,
				"kept", state.kept,
			)
		}
	case seen && previous == state:
		// Unchanged since the last attempt: already reported.
	default:
		reason := state.err
		if reason == "" {
			reason = "dht_" + state.family + "_unavailable"
		}
		logger.Warn("dht starting nodes unavailable",
			"family", state.family,
			"local_addr", state.localAddr,
			"resolved", state.resolved,
			"kept", state.kept,
			"reason", reason,
		)
	}
}

// stop silences the recorder for the rest of the session's life. It blocks
// until a concurrent observe has finished logging its record, which is what
// lets Session.Close promise "no session log after it returns". It is
// idempotent and safe on a nil receiver.
func (r *dhtRecorder) stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
}

func (r *dhtRecorder) snapshot() []DhtFamilyStatus {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]DhtFamilyStatus, 0, len(r.families))
	for _, state := range r.families {
		out = append(out, DhtFamilyStatus{
			Family:    state.family,
			LocalAddr: state.localAddr,
			Resolved:  state.resolved,
			Kept:      state.kept,
			Ready:     state.err == "" && state.kept > 0,
			Error:     state.err,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Family < out[j].Family })
	return out
}

// The upstream bootstrap resolver ignores the network argument, so filter after the socket is known.
func configureDhtStartingNodes(cc *torrent.ClientConfig, recorder *dhtRecorder, logger *slog.Logger) {
	existing := cc.ConfigureAnacrolixDhtServer
	cc.ConfigureAnacrolixDhtServer = func(server *dht.ServerConfig) {
		if existing != nil {
			existing(server)
		}
		if server.Conn == nil || server.StartingNodes == nil {
			return
		}

		network := dhtNetworkForConn(server.Conn)
		localAddr := server.Conn.LocalAddr().String()
		startingNodes := server.StartingNodes
		server.StartingNodes = func() ([]dht.Addr, error) {
			nodes, err := startingNodes()
			if err != nil {
				recorder.observe(logger, dhtFamilyState{family: network, localAddr: localAddr, err: err.Error()})
				return nil, err
			}
			filtered := filterDhtStartingNodes(network, nodes)
			recorder.observe(logger, dhtFamilyState{
				family:    network,
				localAddr: localAddr,
				resolved:  len(nodes),
				kept:      len(filtered),
			})
			return filtered, nil
		}
	}
}

func dhtNetworkForConn(conn net.PacketConn) string {
	return dhtNetworkForAddr(conn.LocalAddr())
}

func dhtNetworkForAddr(addr net.Addr) string {
	udpAddr, ok := addr.(*net.UDPAddr)
	if !ok {
		return addr.Network()
	}
	if udpAddr.IP.To4() != nil {
		return "udp4"
	}
	if udpAddr.IP.To16() != nil {
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

// staticStartingNodes turns explicit bootstrap entries into a starting-node
// getter. Every entry is resolved on each call, so one hostname serving both A
// and AAAA records can feed either socket; the per-socket address-family filter
// then keeps only the family that asked. Replacing the default resolver with
// explicit nodes is the only reliable way out of an environment whose DNS
// answers carry no address of the family a socket needs.
func staticStartingNodes(nodes []string) dht.StartingNodesGetter {
	return func() ([]dht.Addr, error) {
		var addrs []dht.Addr
		for _, node := range nodes {
			host, portText, err := net.SplitHostPort(node)
			if err != nil {
				return nil, fmt.Errorf("session: bootstrap node %q: %w", node, err)
			}
			port, err := strconv.Atoi(portText)
			if err != nil {
				return nil, fmt.Errorf("session: bootstrap node %q: invalid port", node)
			}
			ips, err := net.LookupIP(host)
			if err != nil {
				return nil, fmt.Errorf("session: resolve bootstrap node %q: %w", node, err)
			}
			for _, ip := range ips {
				addrs = append(addrs, dht.NewAddr(&net.UDPAddr{IP: ip, Port: port}))
			}
		}
		return addrs, nil
	}
}
