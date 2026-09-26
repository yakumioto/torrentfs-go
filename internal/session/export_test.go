package session

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/cache"
)

// AddTorrent registers an ephemeral test torrent without writing the managed
// registry. Production adds must use AddTorrentAndPersist.
func (s *Session) AddTorrent(ctx context.Context, src Source) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	st, _, err := s.addTorrentLockedResult(ctx, src)
	if err != nil {
		s.mu.Unlock()
		attrs := []any{"source", sourceKind(src), "err", err}
		if src.MetainfoPath != "" {
			attrs = append(attrs, "path", src.MetainfoPath)
		}
		s.logger.Error("torrent add failed", attrs...)
		return err
	}
	hash := st.InfoHash()
	if s.states[hash] == nil {
		state := StateAdding
		if st.Info() != nil {
			state = StateReady
		}
		s.states[hash] = &registryEntry{
			ID:        hash.HexString(),
			InfoHash:  hash.HexString(),
			Name:      st.Name(),
			State:     state,
			CreatedAt: time.Now().UTC(),
		}
	}
	s.mu.Unlock()
	return nil
}

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

// SetSubtitleCleanupHook installs fn as the forced-failure hook for the
// subtitle cleanup stage of a deletion and returns a function that restores the
// previous value. A nil hook clears it. Test-only.
func SetSubtitleCleanupHook(fn func(metainfo.Hash) error) func() {
	previous := subtitleCleanupHook
	subtitleCleanupHook = fn
	return func() { subtitleCleanupHook = previous }
}

// SetSubtitleIOFault forces one named subtitle upload stage to fail with the
// error the hook returns, and returns a function that clears it. Test-only.
func SetSubtitleIOFault(fn func(stage string) error) func() {
	previous := subtitleIOFault
	subtitleIOFault = fn
	return func() { subtitleIOFault = previous }
}

// SubtitleStageCreate names the staging-file creation stage for tests.
const SubtitleStageCreate = subtitleStageCreate

// SubtitleStageWrite names the payload copy stage for tests.
const SubtitleStageWrite = subtitleStageWrite

// SubtitleStageSync names the fsync/chmod stage for tests.
const SubtitleStageSync = subtitleStageSync

// SubtitleStageRename names the publish rename stage for tests.
const SubtitleStageRename = subtitleStageRename

// SubtitleStageCommit names the post-rename durability stage for tests.
const SubtitleStageCommit = subtitleStageCommit

// SubtitleRootForTest reports the durable root that owns managed subtitles.
// Test-only.
func (s *Session) SubtitleRootForTest() string { return s.subtitleRoot }

// SetMetadataFetchHook installs fn as the post-resolution metadata fetch hook
// and returns a function that restores the previous value. A nil hook clears
// it. Test-only.
func SetMetadataFetchHook(fn func(metainfo.Hash)) func() {
	previous := metadataFetchHook
	metadataFetchHook = fn
	return func() { metadataFetchHook = previous }
}

// DeliverMetadataForTest hands a registered torrent the metainfo a peer would
// have delivered, so the metadata persistence path can be driven without a
// network. Test-only.
func DeliverMetadataForTest(st *Torrent, torrentBytes []byte) error {
	mi, err := metainfo.Load(bytes.NewReader(torrentBytes))
	if err != nil {
		return err
	}
	return UnderlyingTorrentForTest(st).SetInfoBytes(mi.InfoBytes)
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

func unwrapRAFile(reader io.ReaderAt) *raFile {
	switch value := reader.(type) {
	case *raFile:
		return value
	case *openedFile:
		return value.file
	default:
		return nil
	}
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

// UnderlyingTorrentForTest exposes the anacrolix torrent behind a session
// handle so discovery tests can drive it directly, for example to inject peer
// addresses or request every piece. Test-only.
func UnderlyingTorrentForTest(st *Torrent) *torrent.Torrent {
	return st.tor
}

// UnderlyingClientForTest exposes the anacrolix client so discovery tests can
// read Client.WriteStatus, which reports the per-torrent DHT announce counter.
// There is no per-torrent status entry point in this version. Test-only.
//
// This seam exists to reach observation points. Whatever it is used to read
// must be concurrency-safe as read: WriteStatus holds the client's read lock
// for its whole run, so every accessor it reaches has to be synchronized too.
// The tracker segment reaches internal/mytimer.Timer.When(), which upstream
// reads without a lock. v1.61.0-bep27.2 (MIO-46) adds that timer lock; the
// current v1.61.0-bep27.3 pin retains it and adds MIO-62's closed peer-request
// shutdown cleanup.
func UnderlyingClientForTest(s *Session) *torrent.Client {
	return s.cl
}

// PrefetchSnapshot is the test-visible copy of the coordinator's state.
type PrefetchSnapshot = prefetchSnapshot

// EvictPiecesForTest drops every piece of the torrent except the ones in keep
// from the session's cache, simulating the LRU reclaiming a read window. It
// syncs anacrolix's completion view so the pieces read as missing again.
// Test-only.
func (s *Session) EvictPiecesForTest(hash metainfo.Hash, keep ...int) error {
	s.mu.RLock()
	st, ok := s.torrents[hash]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session: evict: unknown torrent %s", hash)
	}
	info := st.tor.Info()
	if info == nil {
		return fmt.Errorf("session: evict: torrent %s has no info", hash)
	}
	kept := make(map[int]struct{}, len(keep))
	for _, index := range keep {
		kept[index] = struct{}{}
	}
	for index := 0; index < info.NumPieces(); index++ {
		if _, ok := kept[index]; ok {
			continue
		}
		s.pieceCache.Remove(cache.Key{Torrent: hash.HexString(), Piece: index})
		st.tor.Piece(index).UpdateCompletion()
	}
	return nil
}

// PrefetchDefaultPiecesForTest returns the compiled-in background-piece limit
// so a test can assert against the implementation's own default. Test-only.
func PrefetchDefaultPiecesForTest() int { return defaultPrefetchPieces }

// SeedPieceForTest loads one verified piece into the session's cache and syncs
// anacrolix's completion view for it, leaving every other piece missing. A test
// uses it to make one foreground read a cache hit while the window ahead stays
// empty. Test-only.
func (s *Session) SeedPieceForTest(hash metainfo.Hash, index int, content []byte) error {
	s.mu.RLock()
	st, ok := s.torrents[hash]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session: seed piece: unknown torrent %s", hash)
	}
	info := st.tor.Info()
	if info == nil || info.PieceLength <= 0 {
		return fmt.Errorf("session: seed piece: torrent %s has no usable info", hash)
	}
	start := int64(index) * info.PieceLength
	end := start + info.PieceLength
	if end > int64(len(content)) {
		end = int64(len(content))
	}
	if index < 0 || start >= end {
		return fmt.Errorf("session: seed piece: piece %d is outside the content", index)
	}
	s.pieceCache.Put(cache.Key{Torrent: hash.HexString(), Piece: index}, content[start:end])
	st.tor.Piece(index).UpdateCompletion()
	return nil
}

// PrefetchSnapshotForTest reports the torrent's prefetch coordinator state, or
// a zero snapshot when no coordinator was created yet. Test-only.
func PrefetchSnapshotForTest(st *Torrent) PrefetchSnapshot {
	st.mu.Lock()
	coordinator := st.coordinator
	st.mu.Unlock()
	if coordinator == nil {
		return PrefetchSnapshot{}
	}
	return coordinator.snapshot()
}

// SetPrefetchBudgetLimitForTest changes the session's global background-piece
// limit and returns a restore function. Test-only.
func SetPrefetchBudgetLimitForTest(s *Session, limit int) func() {
	return s.prefetchBudget.setLimit(limit)
}

// PrefetchBudgetUsageForTest reports the session's (limit, used) token counts.
// Test-only.
func PrefetchBudgetUsageForTest(s *Session) (limit, used int) {
	return s.prefetchBudget.snapshot()
}

// SameRAFile reports whether two reader handles are the same session raFile,
// and whether they share one pieceLoader. Test-only.
func SameRAFile(a, b io.ReaderAt) (sameFile, sameLoader bool) {
	first := unwrapRAFile(a)
	second := unwrapRAFile(b)
	if first == nil || second == nil {
		return false, false
	}
	return first == second, first.loader == second.loader
}
