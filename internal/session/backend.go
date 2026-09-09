package session

import (
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

var (
	_ filesystem.Backend         = (*Session)(nil)
	_ filesystem.MetadataBackend = (*Session)(nil)

	errPieceStateSnapshotUnstable = errors.New("session: piece state snapshot is unstable")
)

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
		view := filesystem.TorrentView{Name: t.Name(), Hash: t.InfoHash()}
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

// PieceStates implements filesystem.Backend with a value snapshot of the
// anacrolix piece state for the requested torrent.
func (s *Session) PieceStates(hash metainfo.Hash) ([]filesystem.PieceState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.ensureActiveLocked(); err != nil {
		return nil, err
	}
	t, ok := s.torrents[hash]
	if !ok {
		return nil, fmt.Errorf("session: unknown torrent %s: %w", hash, filesystem.ErrNotFound)
	}
	info := t.tor.Info()
	if info == nil {
		return nil, fmt.Errorf("session: torrent info is not ready: %w", filesystem.ErrNotFound)
	}

	states, err := pieceStatesSnapshot(t.tor, info)
	if err != nil {
		return nil, err
	}
	return states, nil
}

type pieceStateSource interface {
	PieceStateRuns() torrent.PieceStateRuns
	PieceBytesMissing(int) int64
	Length() int64
}

var _ pieceStateSource = (*torrent.Torrent)(nil)

func pieceStatesSnapshot(t pieceStateSource, info *metainfo.Info) ([]filesystem.PieceState, error) {
	for attempt := 0; attempt < 3; attempt++ {
		runs := t.PieceStateRuns()
		states := make([]filesystem.PieceState, 0)
		for _, run := range runs {
			for range run.Length {
				index := len(states)
				piece := filesystem.PieceState{
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
