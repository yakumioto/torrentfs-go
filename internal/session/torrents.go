package session

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

// Source identifies where the metainfo of a torrent to add comes from.
// Exactly one field must be set.
type Source struct {
	// MetainfoPath is a path to a .torrent file.
	MetainfoPath string
	// MagnetURI is a magnet link whose metadata may still need fetching.
	MagnetURI string
}

// Torrent is a session handle on one registered torrent. It exposes the
// anacrolix state the filesystem layer and tests need.
type Torrent struct {
	tor *torrent.Torrent

	mu      sync.Mutex
	readers map[string]*raFile // open reader handles by display path
}

// AddTorrent registers a torrent from a .torrent file or a magnet link. It
// returns once the torrent is registered with the client; for magnet sources
// the metainfo (and thus Info, Name and Files) may still be pending.
func (s *Session) AddTorrent(ctx context.Context, src Source) error {
	var t *torrent.Torrent
	var err error
	switch {
	case src.MagnetURI != "" && src.MetainfoPath != "":
		return errors.New("session: Source sets both MetainfoPath and MagnetURI")
	case src.MagnetURI != "":
		t, err = s.cl.AddMagnet(src.MagnetURI)
	case src.MetainfoPath != "":
		t, err = s.cl.AddTorrentFromFile(src.MetainfoPath)
	default:
		return errors.New("session: Source needs MetainfoPath or MagnetURI")
	}
	if err != nil {
		return fmt.Errorf("session: add torrent: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.torrents[t.InfoHash()]; !ok {
		s.torrents[t.InfoHash()] = &Torrent{tor: t, readers: make(map[string]*raFile)}
	}
	return nil
}

// Torrent returns the registered torrent with the given info hash.
func (s *Session) Torrent(hash metainfo.Hash) (*Torrent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.torrents[hash]
	return st, ok
}

// List returns every registered torrent, in no particular order.
func (s *Session) List() []*Torrent {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// BytesCompleted returns how many bytes are verified and available locally.
func (t *Torrent) BytesCompleted() int64 {
	return t.tor.BytesCompleted()
}

// readerFor returns the shared reader handle for the file with the given
// display path, opening it on first use. Handles are owned by the session:
// they are closed by Close, never by the filesystem's release path.
func (t *Torrent) readerFor(displayPath string) (*raFile, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r, ok := t.readers[displayPath]; ok {
		return r, nil
	}
	var f *torrent.File
	for _, ff := range t.tor.Files() {
		if ff.DisplayPath() == displayPath {
			f = ff
			break
		}
	}
	if f == nil {
		return nil, fmt.Errorf("session: no file %q in torrent %s", displayPath, t.InfoHash())
	}
	r := &raFile{r: f.NewReader()}
	t.readers[displayPath] = r
	return r, nil
}

// close releases every open reader handle for this torrent.
func (t *Torrent) close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	var errs []error
	for path, r := range t.readers {
		if err := r.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close %q: %w", path, err))
		}
		delete(t.readers, path)
	}
	return errors.Join(errs...)
}
