package session

import (
	"fmt"
	"io"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

var (
	_ filesystem.Backend         = (*Session)(nil)
	_ filesystem.MetadataBackend = (*Session)(nil)
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
