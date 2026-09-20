package session

import (
	"context"
	"fmt"
	"io"
	"slices"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/cache"
)

// SeedPiecesForTest loads content into the session's in-memory piece cache as
// if the pieces had been downloaded and verified, then syncs anacrolix's
// completion view so the torrent can serve reads and uploads. Tests use it in
// place of the old on-disk payload fixture, which no longer exists. Test-only.
func (s *Session) SeedPiecesForTest(hash metainfo.Hash, content []byte) error {
	s.mu.RLock()
	st, ok := s.torrents[hash]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session: seed: unknown torrent %s", hash)
	}
	info := st.tor.Info()
	if info == nil {
		return fmt.Errorf("session: seed: torrent %s has no info", hash)
	}
	if info.PieceLength <= 0 {
		return fmt.Errorf("session: seed: torrent %s has invalid piece length", hash)
	}

	count := info.NumPieces()
	for index := 0; index < count; index++ {
		start := int64(index) * info.PieceLength
		end := start + info.PieceLength
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		if start >= end {
			break
		}
		s.pieceCache.Put(cache.Key{Torrent: hash.HexString(), Piece: index}, content[start:end])
	}
	for index := 0; index < count; index++ {
		st.tor.Piece(index).UpdateCompletion()
	}
	return nil
}

// StartMetadataFetch starts the real magnet metadata-persist worker for a
// torrent whose info is already known, so tests can drive the persistence path
// deterministically. Test-only.
func (s *Session) StartMetadataFetch(st *Torrent) {
	s.startMetadataFetch(st.InfoHash(), st)
}

// PendingMetadataFetches reports how many metadata workers are still tracked.
// Test-only.
func (s *Session) PendingMetadataFetches() int {
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	return len(s.metadataFetches)
}

// SetMetadataFetchHook installs fn as the post-resolution metadata fetch hook
// and returns a function that restores the previous value. A nil hook clears
// it. Test-only.
func SetMetadataFetchHook(fn func(metainfo.Hash)) func() {
	previous := metadataFetchHook
	metadataFetchHook = fn
	return func() { metadataFetchHook = previous }
}

// WriteMetadataForTest exercises the metadata persistence path with a caller
// supplied context. Test-only.
func (s *Session) WriteMetadataForTest(ctx context.Context, hash metainfo.Hash, data []byte) error {
	return s.writeMetadataBytes(ctx, hash, data)
}

// NewWithClientConfig exposes the test-only client configuration seam so
// integration tests can disable discovery traffic (DHT, UTP) and keep every
// connection on the loopback interface. It is not part of the public API.
var NewWithClientConfig = newWithClientConfig

// TorrentClientConfig is the concrete client configuration type tests adjust
// through NewWithClientConfig.
type TorrentClientConfig = torrent.ClientConfig

// PeerIDForTest returns the 20-byte peer ID the client announces with.
// Test-only.
func (s *Session) PeerIDForTest() string {
	id := s.cl.PeerID()
	return string(id[:])
}

// DhtServerFamiliesForTest reports the address family of every DHT server the
// client runs. Test-only.
func (s *Session) DhtServerFamiliesForTest() []string {
	servers := s.cl.DhtServers()
	families := make([]string, 0, len(servers))
	for _, server := range servers {
		families = append(families, dhtNetworkForAddr(server.Addr()))
	}
	slices.Sort(families)
	return families
}

// SetReadaheadForTest changes the session reader's byte window for a cold-read
// integration test. It is not part of the public API.
func SetReadaheadForTest(reader io.ReaderAt, readahead int64) bool {
	file, ok := reader.(*raFile)
	if !ok {
		return false
	}
	file.mu.Lock()
	file.readahead = readahead
	file.mu.Unlock()
	return true
}

// PieceStateRunsForTest exposes the underlying torrent priority/completion
// snapshot to session integration tests. It is not part of the public API.
func PieceStateRunsForTest(st *Torrent) torrent.PieceStateRuns {
	return st.tor.PieceStateRuns()
}

// ReadProbeEvent is one loader read event reported to a test-installed probe.
type ReadProbeEvent = readProbeEvent

// SetReadProbe installs ch as the loader read probe and returns a function that
// restores the previous probe. Events are sent non-blocking to ch, so a test
// must give it enough capacity for every event it expects. A nil channel clears
// the probe. This is a test-only seam; production never installs one.
func SetReadProbe(ch chan ReadProbeEvent) (restore func()) {
	var next *chan readProbeEvent
	if ch != nil {
		next = &ch
	}
	previous := readProbe.Swap(next)
	return func() { readProbe.Store(previous) }
}

// SameRAFile reports whether two reader handles are the same session raFile,
// and whether they share one pieceLoader. Test-only.
func SameRAFile(a, b io.ReaderAt) (sameFile, sameLoader bool) {
	first, ok := a.(*raFile)
	if !ok {
		return false, false
	}
	second, ok := b.(*raFile)
	if !ok {
		return false, false
	}
	return first == second, first.loader == second.loader
}
