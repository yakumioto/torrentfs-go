package session

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/cache"
	"github.com/yakumioto/torrentfs-go/internal/vfs"
)

var (
	_ vfs.Backend                = (*Session)(nil)
	_ vfs.TorrentContentsBackend = (*Session)(nil)

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
	Torrent         TorrentView
	MetainfoReady   bool
	PieceLength     int64
	Pieces          []PieceStatus
	Files           []FileStatus
	Network         NetworkStatus
	SubtitleTargets []SubtitleTarget
	Subtitles       []Subtitle
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

// Torrents implements vfs.Backend. Torrents whose metainfo is not yet
// available (magnet sources still resolving) are excluded: without info there
// is nothing to show or read.
func (s *Session) Torrents() []vfs.TorrentView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state != stateActive {
		return nil
	}
	return s.filesystemViewsLocked()
}

func (s *Session) filesystemViewsLocked() []vfs.TorrentView {
	hashes := make([]metainfo.Hash, 0, len(s.states))
	for hash, entry := range s.states {
		if entry.State == StateReady {
			hashes = append(hashes, hash)
		}
	}
	sort.Slice(hashes, func(i, j int) bool { return bytes.Compare(hashes[i][:], hashes[j][:]) < 0 })
	views := make([]vfs.TorrentView, 0, len(hashes))
	for _, hash := range hashes {
		contents, err := s.torrentContentsLocked(hash)
		if err != nil {
			continue
		}
		views = append(views, vfs.TorrentView{
			Name:       s.torrents[hash].Name(),
			Hash:       hash,
			Category:   s.states[hash].Category,
			CreatedAt:  contents.CreatedAt,
			Files:      contents.Files,
			FilesID:    contents.FilesID,
			Subtitles:  contents.Subtitles,
			SingleFile: contents.SingleFile,
		})
	}
	return views
}

// TorrentContents resolves one current ready torrent without enumerating the
// library. Internal callers that hold mu use torrentContentsLocked instead.
func (s *Session) TorrentContents(hash metainfo.Hash) (vfs.TorrentContents, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.torrentContentsLocked(hash)
}

func (s *Session) torrentContentsLocked(hash metainfo.Hash) (vfs.TorrentContents, error) {
	if err := s.ensureActiveLocked(); err != nil {
		return vfs.TorrentContents{}, err
	}
	entry := s.states[hash]
	t := s.torrents[hash]
	if entry == nil || entry.State != StateReady || t == nil {
		return vfs.TorrentContents{}, vfs.ErrNotFound
	}
	payload := t.payloadSnapshot()
	if payload == nil {
		return vfs.TorrentContents{}, vfs.ErrNotFound
	}
	return vfs.TorrentContents{
		Files:      payload.files,
		FilesID:    payload.id,
		Subtitles:  s.subtitleViewsLocked(hash),
		CreatedAt:  entry.CreatedAt,
		SingleFile: !t.Info().IsDir(),
	}, nil
}

// OpenFile implements vfs.Backend: it returns a reader handle for the
// file at the given display path inside the torrent identified by hash.
func (s *Session) OpenFile(hash metainfo.Hash, path string) (io.ReaderAt, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.ensureActiveLocked(); err != nil {
		return nil, err
	}
	entry, ok := s.states[hash]
	if !ok || entry.State != StateReady {
		return nil, fmt.Errorf("session: unknown torrent %s: %w", hash, vfs.ErrNotFound)
	}
	t, ok := s.torrents[hash]
	if !ok {
		return nil, fmt.Errorf("session: unknown torrent %s: %w", hash, vfs.ErrNotFound)
	}
	return t.readerFor(path)
}

// OpenSubtitle opens a managed subtitle for the FUSE read path. The metadata
// and the file come from the same opened descriptor, so a replacement that lands
// between them cannot pair one version's length with another version's bytes.
func (s *Session) OpenSubtitle(hash metainfo.Hash, path string) (vfs.SubtitleSnapshot, error) {
	rel, err := s.managedSubtitlePath(hash, path)
	if err != nil {
		return vfs.SubtitleSnapshot{}, err
	}
	store := s.subtitleStore
	if store == nil {
		return vfs.SubtitleSnapshot{}, vfs.ErrNotFound
	}
	file, err := store.openRead(rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return vfs.SubtitleSnapshot{}, vfs.ErrNotFound
		}
		return vfs.SubtitleSnapshot{}, err
	}
	if subtitleOpenHook != nil {
		subtitleOpenHook(hash, path)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return vfs.SubtitleSnapshot{}, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return vfs.SubtitleSnapshot{}, vfs.ErrNotFound
	}
	return vfs.SubtitleSnapshot{
		Reader:     file,
		Size:       info.Size(),
		ModifiedAt: info.ModTime().UTC(),
	}, nil
}

// SubtitleStat reports a managed subtitle's current metadata for getattr. It is
// separate from OpenSubtitle because a stat is a point-in-time observation: the
// coherence that matters is between the length and the bytes of one open, which
// OpenSubtitle guarantees on its own.
func (s *Session) SubtitleStat(hash metainfo.Hash, path string) (vfs.SubtitleStat, error) {
	rel, err := s.managedSubtitlePath(hash, path)
	if err != nil {
		return vfs.SubtitleStat{}, err
	}
	store := s.subtitleStore
	if store == nil {
		return vfs.SubtitleStat{}, vfs.ErrNotFound
	}
	stat, err := store.lstat(rel)
	if err != nil || stat.Mode&subtitleStoreFileType != subtitleStoreRegular {
		return vfs.SubtitleStat{}, vfs.ErrNotFound
	}
	return vfs.SubtitleStat{
		Size:       stat.Size,
		ModifiedAt: time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec).UTC(),
	}, nil
}

// managedSubtitlePath resolves one published subtitle to a path relative to the
// managed store, refusing anything the index does not know about.
func (s *Session) managedSubtitlePath(hash metainfo.Hash, path string) (string, error) {
	if err := validateSubtitleRelativePath(path); err != nil {
		return "", vfs.ErrNotFound
	}
	s.mu.RLock()
	activeErr := s.ensureActiveLocked()
	entry := s.states[hash]
	_, known := s.subtitles[hash][path]
	s.mu.RUnlock()
	if activeErr != nil {
		return "", activeErr
	}
	if entry == nil || entry.State != StateReady || !known {
		return "", vfs.ErrNotFound
	}
	return filepath.Join(subtitleDirName(hash), filepath.FromSlash(path)), nil
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
		Torrent:         s.buildViewWithCached(hash, st, entry, cachedBytes),
		Pieces:          make([]PieceStatus, 0),
		Files:           make([]FileStatus, 0),
		SubtitleTargets: make([]SubtitleTarget, 0),
		Subtitles:       make([]Subtitle, 0),
		Network:         s.networkStatus(st),
	}
	if st == nil {
		return view, nil
	}
	info := st.tor.Info()
	if info == nil {
		return view, nil
	}
	if targets, err := s.subtitleTargetsLocked(hash, st); err != nil {
		return TorrentStatusView{}, err
	} else {
		view.SubtitleTargets = targets
	}
	allViews := s.filesystemViewsLocked()
	var currentView vfs.TorrentView
	for _, candidate := range allViews {
		if candidate.Hash == hash {
			currentView = candidate
			break
		}
	}
	categories := s.categoryNamesLocked()
	view.Subtitles = s.subtitlesLocked(hash, currentView, allViews, categories)

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
