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

	"github.com/yakumioto/torrentfs-go/internal/cache"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
)

type lifecycle uint8

const (
	stateActive lifecycle = iota
	stateClosing
	stateClosed
)

// Session owns the anacrolix client and the set of registered torrents.
type Session struct {
	cl  *torrent.Client
	cfg config.Config

	pieceCache     *cache.Cache
	mu             sync.RWMutex
	state          lifecycle
	closeDone      chan struct{}
	closeErr       error
	torrents       map[metainfo.Hash]*Torrent
	metadata       map[string]metainfo.Hash
	metadataRefs   map[metainfo.Hash]int
	pendingWriters map[string]*metadataWriter
	metadataDir    string
}

// New creates a session and its anacrolix client. The data and metadata
// directories are created if missing. The client is configured to seed and
// existing metadata is restored before the session is returned.
func New(cfg config.Config) (*Session, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("session: validate config: %w", err)
	}
	if err := os.MkdirAll(cfg.Paths.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("session: create data dir: %w", err)
	}
	metadataDir := metadataRoot(cfg.Paths.DataDir)
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		return nil, fmt.Errorf("session: create metadata dir: %w", err)
	}

	cc := torrent.NewDefaultClientConfig()
	cc.DataDir = cfg.Paths.DataDir
	cc.ListenHost = func(string) string { return cfg.Connections.ListenHost }
	cc.ListenPort = cfg.Connections.ListenPort
	configureSeeding(cc)
	peerDialer, err := configureProxy(cc, cfg.Proxy.Socks5URL)
	if err != nil {
		return nil, err
	}
	cl, err := torrent.NewClient(cc)
	if err != nil {
		return nil, fmt.Errorf("session: new client: %w", err)
	}
	if peerDialer != nil {
		cl.AddDialer(peerDialer)
	}
	s := &Session{
		cl:             cl,
		cfg:            cfg,
		pieceCache:     cache.New(cfg.Cache.CapacityBytes),
		closeDone:      make(chan struct{}),
		torrents:       make(map[metainfo.Hash]*Torrent),
		metadata:       make(map[string]metainfo.Hash),
		metadataRefs:   make(map[metainfo.Hash]int),
		pendingWriters: make(map[string]*metadataWriter),
		metadataDir:    metadataDir,
	}
	if err := s.rescanMetadata(); err != nil {
		if closeErr := errors.Join(cl.Close()...); closeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("session: close client after metadata restore: %w", closeErr))
		}
		return nil, err
	}
	return s, nil
}

// Close releases every open file handle, drops every torrent, and shuts the
// client down. The underlying client close is synchronous and cannot be
// shortened by ctx.
func (s *Session) Close(ctx context.Context) error {
	_ = ctx

	s.mu.Lock()
	switch s.state {
	case stateClosed:
		err := s.closeErr
		s.mu.Unlock()
		return err
	case stateClosing:
		done := s.closeDone
		s.mu.Unlock()
		<-done
		s.mu.RLock()
		err := s.closeErr
		s.mu.RUnlock()
		return err
	}

	s.state = stateClosing
	torrents := make([]*Torrent, 0, len(s.torrents))
	for _, t := range s.torrents {
		torrents = append(torrents, t)
	}
	writers := make([]*metadataWriter, 0, len(s.pendingWriters))
	for _, w := range s.pendingWriters {
		writers = append(writers, w)
	}
	s.mu.Unlock()

	var errs []error
	for _, w := range writers {
		if err := w.Abort(); err != nil {
			errs = append(errs, fmt.Errorf("abort metadata %q: %w", w.name, err))
		}
	}
	for _, t := range torrents {
		if err := t.close(); err != nil {
			errs = append(errs, err)
		}
		t.tor.Drop()
	}
	for _, err := range s.cl.Close() {
		if err != nil {
			errs = append(errs, err)
		}
	}

	err := errors.Join(errs...)
	s.mu.Lock()
	s.torrents = make(map[metainfo.Hash]*Torrent)
	s.metadata = make(map[string]metainfo.Hash)
	s.metadataRefs = make(map[metainfo.Hash]int)
	s.pendingWriters = make(map[string]*metadataWriter)
	s.state = stateClosed
	s.closeErr = err
	close(s.closeDone)
	s.mu.Unlock()
	return err
}

func (s *Session) ensureActiveLocked() error {
	if s.state != stateActive {
		return fmt.Errorf("session: not active: %w", filesystem.ErrClosed)
	}
	return nil
}
