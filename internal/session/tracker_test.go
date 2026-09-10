package session_test

import (
	"encoding/binary"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/anacrolix/torrent/bencode"
)

// loopbackTracker is a minimal HTTP BitTorrent tracker for integration tests.
// It binds loopback only, ignores the compact/numwant hints of its clients,
// and always answers with a short announce interval and the compact peer list
// it has recorded. Nothing here contacts the public network.
type loopbackTracker struct {
	server *httptest.Server
	url    string

	mu        sync.Mutex
	peers     map[string]map[string]struct{} // info hash hex -> "host:port" set
	announced map[string]int                 // info hash hex -> announce count
	announces map[string][]trackerAnnounce   // info hash hex -> announce details
}

type trackerAnnounce struct {
	UserAgent string
	PeerID    []byte
}

type trackerAnnounceResponse struct {
	Interval   int64  `bencode:"interval"`
	Complete   int64  `bencode:"complete"`
	Incomplete int64  `bencode:"incomplete"`
	Peers      string `bencode:"peers"`
}

func newLoopbackTracker(t *testing.T) *loopbackTracker {
	t.Helper()
	tr := &loopbackTracker{
		peers:     make(map[string]map[string]struct{}),
		announced: make(map[string]int),
		announces: make(map[string][]trackerAnnounce),
	}
	tr.server = httptest.NewServer(http.HandlerFunc(tr.handleAnnounce))
	t.Cleanup(tr.server.Close)
	tr.url = tr.server.URL + "/announce"
	return tr
}

// announceCount reports how many announces the tracker has seen for hash.
func (tr *loopbackTracker) announceCount(hash string) int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.announced[hash]
}

func (tr *loopbackTracker) announceSnapshot(hash string) []trackerAnnounce {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	announces := tr.announces[hash]
	out := make([]trackerAnnounce, len(announces))
	for i, announce := range announces {
		out[i] = trackerAnnounce{
			UserAgent: announce.UserAgent,
			PeerID:    append([]byte(nil), announce.PeerID...),
		}
	}
	return out
}

func (tr *loopbackTracker) handleAnnounce(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	rawHash := query.Get("info_hash")
	if len(rawHash) != 20 {
		http.Error(w, "bad info_hash", http.StatusBadRequest)
		return
	}
	infoHash := hex.EncodeToString([]byte(rawHash))
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		http.Error(w, "bad remote address", http.StatusBadRequest)
		return
	}
	port := query.Get("port")
	if port == "" {
		http.Error(w, "missing port", http.StatusBadRequest)
		return
	}
	addr := net.JoinHostPort(host, port)
	stopped := query.Get("event") == "stopped"
	announce := trackerAnnounce{
		UserAgent: r.UserAgent(),
		PeerID:    append([]byte(nil), query.Get("peer_id")...),
	}

	tr.mu.Lock()
	tr.announces[infoHash] = append(tr.announces[infoHash], announce)
	set := tr.peers[infoHash]
	if set == nil {
		set = make(map[string]struct{})
		tr.peers[infoHash] = set
	}
	if stopped {
		delete(set, addr)
	} else {
		set[addr] = struct{}{}
		tr.announced[infoHash]++
	}
	others := make([]string, 0, len(set))
	for peer := range set {
		if peer != addr {
			others = append(others, peer)
		}
	}
	tr.mu.Unlock()
	sort.Strings(others)

	body, err := bencode.Marshal(trackerAnnounceResponse{
		Interval: 1,
		Peers:    compactPeerList(others),
	})
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write(body)
}

// compactPeerList encodes peers as the BEP 23 compact form: six bytes per
// peer, four for the IPv4 address and two big-endian for the port.
func compactPeerList(addrs []string) string {
	var b strings.Builder
	for _, addr := range addrs {
		host, portText, err := net.SplitHostPort(addr)
		if err != nil {
			continue
		}
		ip := net.ParseIP(host).To4()
		if ip == nil {
			continue
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		var encoded [2]byte
		binary.BigEndian.PutUint16(encoded[:], uint16(port))
		b.Write(ip)
		b.Write(encoded[:])
	}
	return b.String()
}
