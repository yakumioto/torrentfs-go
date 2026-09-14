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

	pieceCache      *cache.Cache
	mu              sync.RWMutex
	state           lifecycle
	closeDone       chan struct{}
	closeErr        error
	torrents        map[metainfo.Hash]*Torrent
	metadata        map[string]metainfo.Hash
	metadataRefs    map[metainfo.Hash]int
	directoryRefs   map[metainfo.Hash]int
	directorySource map[string]torrentDirSource
	manualRefs      map[metainfo.Hash]struct{}
	pendingWriters  map[string]*metadataWriter
	torrentsDir     string
	metadataDir     string
	scanCancel      context.CancelFunc
	scanDone        chan struct{}
}

// New creates a session and its anacrolix client. The torrents directory must
// already exist; the session creates only its .metadata directory. The client
// is configured to seed, existing metadata is restored, and the torrents
// directory is watched before the session is returned.
func New(cfg config.Config, torrentsDir string) (*Session, error) {
	return newWithClientConfig(cfg, torrentsDir, nil)
}

func newWithClientConfig(cfg config.Config, torrentsDir string, customize func(*torrent.ClientConfig)) (*Session, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("session: validate config: %w", err)
	}
	var err error
	torrentsDir, err = validateTorrentDir(torrentsDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.Paths.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("session: create data dir: %w", err)
	}
	metadataDir := metadataRoot(torrentsDir)
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		return nil, fmt.Errorf("session: create metadata dir: %w", err)
	}

	cc := torrent.NewDefaultClientConfig()
	if cfg.Identity.TrackerUserAgent != "" {
		cc.HTTPUserAgent = cfg.Identity.TrackerUserAgent
	}
	if cfg.Identity.PeerIDPrefix != "" {
		cc.Bep20 = cfg.Identity.PeerIDPrefix
	}
	if cfg.Identity.ExtendedHandshakeClientVersion != "" {
		cc.ExtendedHandshakeClientVersion = cfg.Identity.ExtendedHandshakeClientVersion
	}
	cc.DataDir = cfg.Paths.DataDir
	cc.ListenHost = func(string) string { return cfg.Connections.ListenHost }
	cc.ListenPort = cfg.Connections.ListenPort
	configureSeeding(cc)
	peerDialer, err := configureProxy(cc, cfg.Proxy.Socks5URL)
	if err != nil {
		return nil, err
	}
	if customize != nil {
		customize(cc)
	}
	cl, err := torrent.NewClient(cc)
	if err != nil {
		return nil, fmt.Errorf("session: new client: %w", err)
	}
	if peerDialer != nil {
		cl.AddDialer(peerDialer)
	}
	s := &Session{
		cl:              cl,
		cfg:             cfg,
		pieceCache:      cache.New(cfg.Cache.CapacityBytes),
		closeDone:       make(chan struct{}),
		torrents:        make(map[metainfo.Hash]*Torrent),
		metadata:        make(map[string]metainfo.Hash),
		metadataRefs:    make(map[metainfo.Hash]int),
		directoryRefs:   make(map[metainfo.Hash]int),
		directorySource: make(map[string]torrentDirSource),
		manualRefs:      make(map[metainfo.Hash]struct{}),
		pendingWriters:  make(map[string]*metadataWriter),
		torrentsDir:     torrentsDir,
		metadataDir:     metadataDir,
	}
	if err := s.rescanMetadata(); err != nil {
		if closeErr := s.Close(context.Background()); closeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("session: close client after metadata restore: %w", closeErr))
		}
		return nil, err
	}
	if err := s.scanTorrentDir(context.Background(), true); err != nil {
		if closeErr := s.Close(context.Background()); closeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("session: close client after torrent scan: %w", closeErr))
		}
		return nil, err
	}
	scanCtx, scanCancel := context.WithCancel(context.Background())
	s.scanCancel = scanCancel
	s.scanDone = make(chan struct{})
	go s.watchTorrentDir(scanCtx, s.scanDone)
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
	scanCancel := s.scanCancel
	scanDone := s.scanDone
	s.mu.Unlock()

	if scanCancel != nil {
		scanCancel()
		if scanDone != nil {
			<-scanDone
		}
	}

	s.mu.Lock()
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
	s.directoryRefs = make(map[metainfo.Hash]int)
	s.directorySource = make(map[string]torrentDirSource)
	s.manualRefs = make(map[metainfo.Hash]struct{})
	s.pendingWriters = make(map[string]*metadataWriter)
	s.scanCancel = nil
	s.scanDone = nil
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
