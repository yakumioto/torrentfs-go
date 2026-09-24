package session

import (
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/cache"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

var (
	_ filesystem.Backend = (*Session)(nil)

	errPieceRange = errors.New("session: invalid piece range")
)

// PieceStatus is one absolute, zero-based piece's cache state in a torrent
// status snapshot. It describes what the cache holds right now, never how much
// of the piece anacrolix believes it has downloaded.
type PieceStatus struct {
	Index       int
	Cached      bool
	CachedBytes int64
	Pinned      bool
}

// FileStatus maps one torrent file to its half-open range in a status
// snapshot's Pieces array.
type FileStatus struct {
	Path       string
	Size       int64
	PieceStart int
	PieceEnd   int
}

// TorrentStatusView is one consistent status snapshot for a torrent.
type TorrentStatusView struct {
	Torrent       TorrentView
	MetainfoReady bool
	PieceLength   int64
	Pieces        []PieceStatus
	Files         []FileStatus
	Network       NetworkStatus
}

// NetworkStatus is the client-visible network state behind a status snapshot.
// It is reporting only: the torrent's ready state stays what it always was,
// because "ready" means the metainfo is loaded, never that peers exist.
type NetworkStatus struct {
	// EffectiveListenPort is the port the client actually listens on, which
	// differs from the configured port whenever that port is 0.
	EffectiveListenPort int
	// TotalPeers, PendingPeers, ActivePeers and ConnectedSeeders are the
	// torrent's peer gauges; PiecesComplete counts verified pieces.
	TotalPeers       int
	PendingPeers     int
	ActivePeers      int
	ConnectedSeeders int
	PiecesComplete   int
	// DhtFamilies reports each address family's DHT view. It is empty when the
	// client runs without DHT.
	DhtFamilies []DhtFamilyStatus
}

// Torrents implements filesystem.Backend. Torrents whose metainfo is not yet
// available (magnet sources still resolving) are excluded: without info there
// is nothing to show or read.
func (s *Session) Torrents() []filesystem.TorrentView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state != stateActive {
		return nil
	}
	hashes := make([]metainfo.Hash, 0, len(s.states))
	for hash, entry := range s.states {
		if entry.State == StateReady {
			hashes = append(hashes, hash)
		}
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].HexString() < hashes[j].HexString() })
	views := make([]filesystem.TorrentView, 0, len(hashes))
	for _, hash := range hashes {
		t := s.torrents[hash]
		if t == nil {
			continue
		}
		info := t.Info()
		if info == nil {
			continue
		}
		entry := s.states[hash]
		view := filesystem.TorrentView{
			Name:       t.Name(),
			Hash:       hash,
			CreatedAt:  entry.CreatedAt,
			SingleFile: !info.IsDir(),
		}
		for _, f := range t.tor.Files() {
			view.Files = append(view.Files, filesystem.FileView{
				Path: f.DisplayPath(),
				Size: f.Length(),
			})
		}
		views = append(views, view)
	}
	return views
}

// OpenFile implements filesystem.Backend: it returns a reader handle for the
// file at the given display path inside the torrent identified by hash.
func (s *Session) OpenFile(hash metainfo.Hash, path string) (io.ReaderAt, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.ensureActiveLocked(); err != nil {
		return nil, err
	}
	entry, ok := s.states[hash]
	if !ok || entry.State != StateReady {
		return nil, fmt.Errorf("session: unknown torrent %s: %w", hash, filesystem.ErrNotFound)
	}
	t, ok := s.torrents[hash]
	if !ok {
		return nil, fmt.Errorf("session: unknown torrent %s: %w", hash, filesystem.ErrNotFound)
	}
	return t.readerFor(path)
}

// TorrentStatusFor returns one fresh, consistent status snapshot for id.
func (s *Session) TorrentStatusFor(id string) (TorrentStatusView, error) {
	hash, err := parseInfoHash(id)
	if err != nil {
		return TorrentStatusView{}, ErrUnknownTorrent
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.ensureActiveLocked(); err != nil {
		return TorrentStatusView{}, err
	}
	st, ok := s.torrents[hash]
	entry := s.states[hash]
	if !ok && entry == nil {
		return TorrentStatusView{}, ErrUnknownTorrent
	}

	// One cache snapshot supplies both the torrent's aggregate cached_bytes and
	// every per-piece cached_bytes, so the response is internally consistent
	// even while the cache is being evicted or filled underneath it.
	cached, pinned := s.pieceCache.Snapshot(hash.HexString())
	var cachedBytes int64
	for _, size := range cached {
		cachedBytes += size
	}

	view := TorrentStatusView{
		Torrent: s.buildViewWithCached(hash, st, entry, cachedBytes),
		Pieces:  make([]PieceStatus, 0),
		Files:   make([]FileStatus, 0),
		Network: s.networkStatus(st),
	}
	if st == nil {
		return view, nil
	}
	info := st.tor.Info()
	if info == nil {
		return view, nil
	}

	pieceCount := info.NumPieces()
	view.MetainfoReady = true
	view.PieceLength = info.PieceLength
	view.Pieces = make([]PieceStatus, pieceCount)
	for index := range pieceCount {
		size, ok := cached[index]
		view.Pieces[index] = PieceStatus{
			Index:       index,
			Cached:      ok,
			CachedBytes: size,
			Pinned:      pinned[index],
		}
	}
	for _, f := range st.tor.Files() {
		start, end, err := filePieceRange(f, info, st.tor.Length(), pieceCount)
		if err != nil {
			return TorrentStatusView{}, err
		}
		view.Files = append(view.Files, FileStatus{
			Path:       f.DisplayPath(),
			Size:       f.Length(),
			PieceStart: start,
			PieceEnd:   end,
		})
	}
	return view, nil
}

// networkStatus snapshots the client's listen port, the torrent's peer gauges,
// and each address family's DHT view. st may be nil: the client-level parts are
// still reported for a torrent that is not registered yet.
func (s *Session) networkStatus(st *Torrent) NetworkStatus {
	status := NetworkStatus{
		EffectiveListenPort: s.cl.LocalPort(),
		DhtFamilies:         s.dhtStatus(),
	}
	if st == nil {
		return status
	}
	gauges := st.tor.Stats().TorrentGauges
	status.TotalPeers = gauges.TotalPeers
	status.PendingPeers = gauges.PendingPeers
	status.ActivePeers = gauges.ActivePeers
	status.ConnectedSeeders = gauges.ConnectedSeeders
	status.PiecesComplete = gauges.PiecesComplete
	return status
}

// dhtStatus merges the last recorded bootstrap outcome per family with the live
// node counts of each running DHT server, so a caller can tell "no server for
// this family" from "server with an empty routing table".
func (s *Session) dhtStatus() []DhtFamilyStatus {
	families := s.dhtRecorder.snapshot()
	index := make(map[string]int, len(families))
	for i, family := range families {
		index[family.Family] = i
	}
	for _, server := range s.cl.DhtServers() {
		family := dhtNetworkForAddr(server.Addr())
		i, ok := index[family]
		if !ok {
			families = append(families, DhtFamilyStatus{Family: family})
			i = len(families) - 1
			index[family] = i
		}
		families[i].LocalAddr = server.Addr().String()
		if stats, ok := server.Stats().(dht.ServerStats); ok {
			families[i].Nodes = stats.Nodes
			families[i].GoodNodes = stats.GoodNodes
		}
	}
	sort.Slice(families, func(i, j int) bool { return families[i].Family < families[j].Family })
	return families
}

func filePieceRange(f *torrent.File, info *metainfo.Info, torrentLength int64, pieceCount int) (int, int, error) {
	if info.PieceLength <= 0 {
		return 0, 0, errPieceRange
	}
	plan := cache.Plan(cache.ReadRequest{
		FileOffset:    0,
		Length:        f.Length(),
		FileStart:     f.Offset(),
		FileSize:      f.Length(),
		PieceLength:   info.PieceLength,
		TorrentLength: torrentLength,
	})
	if len(plan.Wanted) == 0 {
		at := f.Offset() / info.PieceLength
		if at < 0 {
			return 0, 0, errPieceRange
		}
		if at > int64(pieceCount) {
			at = int64(pieceCount)
		}
		return int(at), int(at), nil
	}
	first, last := plan.Wanted[0], plan.Wanted[len(plan.Wanted)-1]+1
	if first < 0 || last > pieceCount || first >= last {
		return 0, 0, errPieceRange
	}
	return first, last, nil
}

// fileByDisplayPath returns the torrent file with the given display path, or
// nil when the torrent has no such file.
func fileByDisplayPath(t *torrent.Torrent, displayPath string) *torrent.File {
	for _, f := range t.Files() {
		if f.DisplayPath() == displayPath {
			return f
		}
	}
	return nil
}
