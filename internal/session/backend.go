package session

import (
	"errors"
	"fmt"
	"io"

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
	views := make([]filesystem.TorrentView, 0, len(s.torrents))
	for _, t := range s.torrents {
		info := t.Info()
		if info == nil {
			continue
		}
		view := filesystem.TorrentView{Name: t.Name(), Hash: t.InfoHash(), SingleFile: !info.IsDir()}
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

	view := TorrentStatusView{
		Torrent: s.buildView(hash, st, entry),
		Pieces:  make([]PieceStatus, 0),
		Files:   make([]FileStatus, 0),
	}
	if st == nil {
		return view, nil
	}
	info := st.tor.Info()
	if info == nil {
		return view, nil
	}

	cached, pinned := s.pieceCache.Snapshot(hash.HexString())
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
