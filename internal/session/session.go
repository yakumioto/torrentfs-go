// Package session owns the BitTorrent client and the torrents registered on
// it. It hides anacrolix behind the small API the rest of torrentfs needs and
// implements filesystem.Backend, so main can inject a session straight into
// the filesystem layer.
package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/yakumioto/torrentfs-go/internal/config"
)

// Session owns the anacrolix client and the set of registered torrents.
type Session struct {
	cl  *torrent.Client
	cfg config.Config

	mu       sync.Mutex
	torrents map[metainfo.Hash]*Torrent
}

// New creates a session and its anacrolix client. The data directory is
// created if missing. The client never seeds and never uploads; it otherwise
// uses anacrolix defaults.
func New(cfg config.Config) (*Session, error) {
	if err := os.MkdirAll(cfg.Paths.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("session: create data dir: %w", err)
	}
	cc := torrent.NewDefaultClientConfig()
	cc.DataDir = cfg.Paths.DataDir
	cc.ListenHost = func(string) string { return cfg.Connections.ListenHost }
	cc.ListenPort = cfg.Connections.ListenPort
	cc.Seed = false
	cc.NoUpload = true
	cl, err := torrent.NewClient(cc)
	if err != nil {
		return nil, fmt.Errorf("session: new client: %w", err)
	}
	return &Session{
		cl:       cl,
		cfg:      cfg,
		torrents: make(map[metainfo.Hash]*Torrent),
	}, nil
}

// Close releases every open file handle and shuts the client down. The
// context is accepted for API symmetry with future cancellable teardown; the
// underlying client close is not cancellable.
func (s *Session) Close(ctx context.Context) error {
	torrents := s.List()
	var errs []error
	for _, t := range torrents {
		if err := t.close(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, err := range s.cl.Close() {
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
