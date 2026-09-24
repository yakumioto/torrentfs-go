package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/cache"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

// Source identifies where the metainfo of a torrent to add comes from.
// Exactly one field must be set.
type Source struct {
	// MetainfoPath is a path to a .torrent file.
	MetainfoPath string
	// MagnetURI is a magnet link whose metadata may still need fetching.
	MagnetURI string
	// Metainfo is raw .torrent bytes, used by callers that already hold the
	// file contents (for example an HTTP upload).
	Metainfo []byte
}

// Torrent is a session handle on one registered torrent. It exposes the
// anacrolix state the filesystem layer and tests need.
type Torrent struct {
	tor     *torrent.Torrent
	session *Session

	mu          sync.Mutex
	closed      bool
	readers     map[string]*raFile // shared reader state by display path
	cache       *cache.Cache
	loader      pieceSource
	coordinator *prefetchCoordinator
}

func sourceKind(src Source) string {
	switch {
	case src.MetainfoPath != "":
		return "metainfo"
	case src.MagnetURI != "":
		return "magnet"
	case len(src.Metainfo) > 0:
		return "metainfo-bytes"
	default:
		return "unknown"
	}
}

func (s *Session) addTorrentLocked(ctx context.Context, src Source) error {
	_, _, err := s.addTorrentLockedResult(ctx, src)
	return err
}

func (s *Session) addTorrentLockedResult(ctx context.Context, src Source) (*Torrent, bool, error) {
	if err := s.ensureActiveLocked(); err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, fmt.Errorf("session: add torrent: %w", err)
	}

	spec, err := specFromSource(src)
	if err != nil {
		return nil, false, err
	}
	return s.addTorrentSpecLocked(ctx, spec)
}

// specFromSource builds a torrent spec from exactly one of a .torrent path, a
// magnet link, or raw metainfo bytes.
func specFromSource(src Source) (*torrent.TorrentSpec, error) {
	set := 0
	if src.MetainfoPath != "" {
		set++
	}
	if src.MagnetURI != "" {
		set++
	}
	if len(src.Metainfo) > 0 {
		set++
	}
	if set != 1 {
		return nil, errors.New("session: Source must set exactly one of MetainfoPath, MagnetURI, or Metainfo")
	}

	if src.MagnetURI != "" {
		spec, err := torrent.TorrentSpecFromMagnetUri(src.MagnetURI)
		if err != nil {
			return nil, fmt.Errorf("session: add torrent: %w", err)
		}
		return spec, nil
	}
	var mi *metainfo.MetaInfo
	var err error
	if src.MetainfoPath != "" {
		mi, err = metainfo.LoadFromFile(src.MetainfoPath)
	} else {
		mi, err = metainfo.Load(bytes.NewReader(src.Metainfo))
	}
	if err != nil {
		return nil, fmt.Errorf("session: add torrent: %w", err)
	}
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		return nil, fmt.Errorf("session: add torrent: %w", err)
	}
	return spec, nil
}

func (s *Session) prepareTorrentSpecLocked(ctx context.Context, spec *torrent.TorrentSpec) (*Torrent, bool, error) {
	if err := s.ensureActiveLocked(); err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, fmt.Errorf("session: add torrent: %w", err)
	}
	if s.deletionPendingLocked(spec.InfoHash) {
		return nil, false, fmt.Errorf("%w: %s", ErrDeleting, spec.InfoHash)
	}
	if s.cfg.Proxy.Socks5URL != "" {
		spec.Trackers = filterProxyTrackers(spec.Trackers)
	}

	t, clientNew, err := s.cl.AddTorrentSpec(spec)
	if err != nil {
		return nil, false, fmt.Errorf("session: add torrent: %w", err)
	}
	if err := ctx.Err(); err != nil {
		if clientNew {
			t.Drop()
		}
		return nil, false, fmt.Errorf("session: add torrent: %w", err)
	}

	hash := t.InfoHash()
	if existing, ok := s.torrents[hash]; ok {
		if existing.tor != t && clientNew {
			t.Drop()
		}
		return existing, false, nil
	}
	return &Torrent{tor: t, session: s, readers: make(map[string]*raFile), cache: s.pieceCache}, true, nil
}

func (s *Session) publishTorrentLocked(hash metainfo.Hash, st *Torrent) {
	s.torrents[hash] = st
	delete(s.lastOps, hash)
}

func (s *Session) addTorrentSpecLocked(ctx context.Context, spec *torrent.TorrentSpec) (*Torrent, bool, error) {
	st, clientNew, err := s.prepareTorrentSpecLocked(ctx, spec)
	if err != nil {
		return nil, false, err
	}
	if clientNew {
		s.publishTorrentLocked(st.InfoHash(), st)
	}
	return st, clientNew, nil
}

// Torrent returns the registered torrent with the given info hash.
func (s *Session) Torrent(hash metainfo.Hash) (*Torrent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.torrents[hash]
	return st, ok
}

// List returns every registered torrent, in no particular order.
func (s *Session) List() []*Torrent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Torrent, 0, len(s.torrents))
	for _, st := range s.torrents {
		out = append(out, st)
	}
	return out
}

// InfoHash returns the torrent's info hash.
func (t *Torrent) InfoHash() metainfo.Hash {
	return t.tor.InfoHash()
}

// GotInfo returns a channel that is closed once the torrent's metainfo is
// available.
func (t *Torrent) GotInfo() <-chan struct{} {
	return t.tor.GotInfo()
}

// Info returns the parsed metainfo, or nil until it is available.
func (t *Torrent) Info() *metainfo.Info {
	return t.tor.Info()
}

// Name returns the torrent's display name (empty until info is known).
func (t *Torrent) Name() string {
	return t.tor.Name()
}

// Length returns the total length of the torrent in bytes (0 until info is
// known).
func (t *Torrent) Length() int64 {
	return t.tor.Length()
}

// CachedBytes returns how many of this torrent's bytes are resident in the
// piece cache. It is a point-in-time occupancy, not a download count: it falls
// as pieces are evicted.
func (t *Torrent) CachedBytes() int64 {
	if t.cache == nil {
		return 0
	}
	return t.cache.SizeOf(t.InfoHash().HexString())
}

// readerFor returns a handle on the file with the given display path. The
// heavy reader state (fetcher, coordinator, read window) is shared by every
// open of that path; the returned handle is cheap and idempotently closable, so
// the filesystem's release path can drop it without disturbing other opens.
// Once the last handle closes, the shared forward window is released.
func (t *Torrent) readerFor(displayPath string) (*openedFile, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, fmt.Errorf("session: torrent is closed: %w", filesystem.ErrClosed)
	}
	if r, ok := t.readers[displayPath]; ok {
		demandID, ok := r.acquireHandle()
		if !ok {
			return nil, fmt.Errorf("session: torrent is closed: %w", filesystem.ErrClosed)
		}
		return &openedFile{file: r, demandID: demandID}, nil
	}
	f := fileByDisplayPath(t.tor, displayPath)
	if f == nil {
		return nil, fmt.Errorf("session: no file %q in torrent %s: %w", displayPath, t.InfoHash(), filesystem.ErrNotFound)
	}
	info := t.tor.Info()
	if info == nil {
		return nil, fmt.Errorf("session: torrent info is not ready: %w", filesystem.ErrNotFound)
	}
	if t.loader == nil {
		t.loader = newPieceFetcher(t.tor, t.cache)
	}
	if t.coordinator == nil {
		t.coordinator = newPrefetchCoordinator(t.session, t.tor, t.cache)
	}
	r := &raFile{
		loader:      t.loader,
		cache:       t.cache,
		coordinator: t.coordinator,
		torrentKey:  t.InfoHash().HexString(),
		fileOffset:  f.Offset(),
		fileSize:    f.Length(),
		pieceLength: info.PieceLength,
		torrentSize: t.tor.Length(),
		readahead:   defaultStreamingReadahead,
	}
	demandID, ok := r.acquireHandle()
	if !ok {
		return nil, fmt.Errorf("session: torrent is closed: %w", filesystem.ErrClosed)
	}
	t.readers[displayPath] = r
	return &openedFile{file: r, demandID: demandID}, nil
}

// close releases every open reader handle for this torrent.
func (t *Torrent) close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	readers := make(map[string]*raFile, len(t.readers))
	for path, r := range t.readers {
		readers[path] = r
	}
	t.readers = make(map[string]*raFile)
	loader := t.loader
	coordinator := t.coordinator
	t.mu.Unlock()

	var errs []error
	if coordinator != nil {
		coordinator.close()
	}
	for path, r := range readers {
		if err := r.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close %q: %w", path, err))
		}
	}
	if loader != nil {
		if err := loader.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close torrent reader: %w", err))
		}
	}
	if t.session != nil && t.session.pieceStore != nil {
		t.session.pieceStore.CloseTorrent(t.InfoHash().HexString())
	}
	if t.cache != nil {
		t.cache.InvalidateTorrent(t.InfoHash().HexString())
	}
	return errors.Join(errs...)
}
