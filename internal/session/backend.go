package session

import (
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/cache"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

var (
	_ filesystem.Backend = (*Session)(nil)

	errPieceStateSnapshotUnstable = errors.New("session: piece state snapshot is unstable")
)

// PieceStatus is one absolute, zero-based piece state in a torrent status
// snapshot.
type PieceStatus struct {
	Index          int
	Known          bool
	Complete       bool
	Partial        bool
	Wanted         bool
	Checking       bool
	AvailableBytes *int64
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

type pieceState struct {
	Known    bool
	Complete bool
	Partial  bool
	Wanted   bool
	Checking bool
	Bytes    int64
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
		Torrent: buildView(hash, st, entry),
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

	states, err := pieceStatesSnapshot(st.tor, info)
	if err != nil {
		return TorrentStatusView{}, err
	}
	view.MetainfoReady = true
	view.PieceLength = info.PieceLength
	view.Pieces = make([]PieceStatus, len(states))
	for index, state := range states {
		piece := PieceStatus{
			Index:    index,
			Known:    state.Known,
			Complete: state.Complete,
			Partial:  state.Partial,
			Wanted:   state.Wanted,
			Checking: state.Checking,
		}
		if state.Known && state.Partial {
			available := state.Bytes
			piece.AvailableBytes = &available
		}
		view.Pieces[index] = piece
	}
	for _, f := range st.tor.Files() {
		start, end, err := filePieceRange(f, info, st.tor.Length(), len(states))
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
		return 0, 0, errPieceStateSnapshotUnstable
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
			return 0, 0, errPieceStateSnapshotUnstable
		}
		if at > int64(pieceCount) {
			at = int64(pieceCount)
		}
		return int(at), int(at), nil
	}
	first, last := plan.Wanted[0], plan.Wanted[len(plan.Wanted)-1]+1
	if first < 0 || last > pieceCount || first >= last {
		return 0, 0, errPieceStateSnapshotUnstable
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

type pieceStateSource interface {
	PieceStateRuns() torrent.PieceStateRuns
	PieceBytesMissing(int) int64
	Length() int64
}

var _ pieceStateSource = (*torrent.Torrent)(nil)

func pieceStatesSnapshot(t pieceStateSource, info *metainfo.Info) ([]pieceState, error) {
	for attempt := 0; attempt < 3; attempt++ {
		runs := t.PieceStateRuns()
		states := make([]pieceState, 0)
		for _, run := range runs {
			for range run.Length {
				index := len(states)
				piece := pieceState{
					Known:    run.Ok,
					Complete: run.Complete,
					Partial:  run.Partial,
					Wanted:   run.Priority != torrent.PiecePriorityNone,
					Checking: run.Checking || run.Hashing || run.QueuedForHash || run.Marking || run.MissingPieceLayerHash,
				}
				if piece.Known && piece.Partial {
					piece.Bytes = availablePieceBytes(t, info, index)
				}
				states = append(states, piece)
			}
		}
		if !reflect.DeepEqual(runs, t.PieceStateRuns()) {
			continue
		}
		stable := true
		for index, state := range states {
			if state.Known && state.Partial && availablePieceBytes(t, info, index) != state.Bytes {
				stable = false
				break
			}
		}
		if stable {
			return states, nil
		}
	}
	return nil, errPieceStateSnapshotUnstable
}

func availablePieceBytes(t pieceStateSource, info *metainfo.Info, index int) int64 {
	pieceLength := info.PieceLength
	pieceStart := int64(index) * pieceLength
	if remaining := t.Length() - pieceStart; remaining < pieceLength {
		pieceLength = remaining
	}
	if pieceLength <= 0 {
		return 0
	}
	missing := t.PieceBytesMissing(index)
	available := pieceLength - missing
	if available < 0 {
		return 0
	}
	if available > pieceLength {
		return pieceLength
	}
	return available
}
