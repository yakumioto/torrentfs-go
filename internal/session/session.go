// Package session owns the BitTorrent client and the torrents registered on
// it. It hides anacrolix behind the small API the rest of torrentfs needs and
// implements filesystem.Backend, so main can inject a session straight into
// the filesystem layer.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/yakumioto/torrentfs-go/internal/cache"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/filesystem"
	"github.com/yakumioto/torrentfs-go/internal/logging"
	"github.com/yakumioto/torrentfs-go/internal/piecestore"
)

type lifecycle uint8

const (
	stateActive lifecycle = iota
	stateClosing
	stateClosed
)

// Option configures a Session.
type Option func(*sessionOptions)

type sessionOptions struct {
	logger *slog.Logger
}

// WithLogger routes session and anacrolix client logs to l.
func WithLogger(l *slog.Logger) Option {
	return func(options *sessionOptions) {
		options.logger = l
	}
}

// Session owns the anacrolix client and the set of registered torrents.
type Session struct {
	cl     *torrent.Client
	cfg    config.Config
	logger *slog.Logger

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
	pendingMagnets  map[metainfo.Hash]managedMagnet
	torrentsDir     string
	metadataDir     string
	scanCancel      context.CancelFunc
	scanDone        chan struct{}

	// storageCloser owns the piece store the client does not close on its own
	// when DefaultStorage is set.
	storageCloser storage.ClientImplCloser

	// stateDir holds the durable per-torrent state sidecars that let the
	// session resume an interrupted deletion after a restart.
	stateDir string

	// states and operations back the torrent management API. states mirrors
	// the durable sidecars; operations are in-memory deletion records.
	states     map[metainfo.Hash]*registryEntry
	operations map[string]*Operation
	activeOps  map[metainfo.Hash]string
	lastOps    map[metainfo.Hash]string

	bgCtx    context.Context
	bgCancel context.CancelFunc
	bgWg     sync.WaitGroup

	// metadataFetches tracks the per-hash magnet metadata-persist workers so a
	// deletion can cancel exactly its own hash and wait for it to exit.
	fetchMu         sync.Mutex
	metadataFetches map[metainfo.Hash]*metadataFetch

	opMu    sync.Mutex
	opLocks map[metainfo.Hash]*sync.Mutex
}

// metadataFetch is one in-flight magnet metadata-persist worker.
type metadataFetch struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// New creates a session and its anacrolix client. The torrents directory must
// already exist; the session creates only its .metadata directory. The client
// is configured to seed, existing metadata is restored, and the torrents
// directory is watched before the session is returned.
func New(cfg config.Config, torrentsDir string, opts ...Option) (*Session, error) {
	return newWithClientConfig(cfg, torrentsDir, nil, opts...)
}

func newWithClientConfig(cfg config.Config, torrentsDir string, customize func(*torrent.ClientConfig), opts ...Option) (*Session, error) {
	options := sessionOptions{}
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	logger := options.logger
	forwardLogger := logger != nil
	if logger == nil {
		logger = logging.Discard()
	}
	logInitFailure := func(stage string, err error) {
		logger.Error("session init failed", "stage", stage, "err", err)
	}

	if err := cfg.Validate(); err != nil {
		logInitFailure("validate-config", err)
		return nil, fmt.Errorf("session: validate config: %w", err)
	}
	var err error
	torrentsDir, err = validateTorrentDir(torrentsDir)
	if err != nil {
		logInitFailure("validate-torrents-dir", err)
		return nil, err
	}
	if err := os.MkdirAll(cfg.Paths.DataDir, 0o755); err != nil {
		err = fmt.Errorf("session: create data dir: %w", err)
		logInitFailure("create-data-dir", err)
		return nil, err
	}
	metadataDir := metadataRoot(torrentsDir)
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		err = fmt.Errorf("session: create metadata dir: %w", err)
		logInitFailure("create-metadata-dir", err)
		return nil, err
	}
	warnLegacyPayloadDir(cfg.Paths.DataDir, logger)
	stateDir := filepath.Join(cfg.Paths.DataDir, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		err = fmt.Errorf("session: create state dir: %w", err)
		logInitFailure("create-state-dir", err)
		return nil, err
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
	// Pieces live only in memory: the store keeps them in the LRU cache until
	// they are evicted, and never writes them to disk.
	pieceCache := cache.New(cfg.Cache.CapacityBytes)
	pieceStore := piecestore.New(pieceCache, logger)
	cc.DefaultStorage = pieceStore
	if forwardLogger {
		cc.Slogger = logger
	}
	peerDialer, err := configureProxy(cc, cfg.Proxy.Socks5URL)
	if err != nil {
		logInitFailure("configure-proxy", err)
		return nil, err
	}
	if customize != nil {
		customize(cc)
	}
	cl, err := torrent.NewClient(cc)
	if err != nil {
		err = fmt.Errorf("session: new client: %w", err)
		logInitFailure("new-client", err)
		return nil, err
	}
	if peerDialer != nil {
		cl.AddDialer(peerDialer)
	}
	s := &Session{
		cl:              cl,
		cfg:             cfg,
		logger:          logger,
		pieceCache:      pieceCache,
		closeDone:       make(chan struct{}),
		torrents:        make(map[metainfo.Hash]*Torrent),
		metadata:        make(map[string]metainfo.Hash),
		metadataRefs:    make(map[metainfo.Hash]int),
		directoryRefs:   make(map[metainfo.Hash]int),
		directorySource: make(map[string]torrentDirSource),
		manualRefs:      make(map[metainfo.Hash]struct{}),
		pendingMagnets:  make(map[metainfo.Hash]managedMagnet),
		torrentsDir:     torrentsDir,
		metadataDir:     metadataDir,
		storageCloser:   pieceStore,
		stateDir:        stateDir,
		states:          make(map[metainfo.Hash]*registryEntry),
		operations:      make(map[string]*Operation),
		activeOps:       make(map[metainfo.Hash]string),
		lastOps:         make(map[metainfo.Hash]string),
		opLocks:         make(map[metainfo.Hash]*sync.Mutex),
		metadataFetches: make(map[metainfo.Hash]*metadataFetch),
	}
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	if err := s.rescanMetadata(); err != nil {
		initErr := err
		if closeErr := s.Close(context.Background()); closeErr != nil {
			initErr = errors.Join(err, fmt.Errorf("session: close client after metadata restore: %w", closeErr))
		}
		logInitFailure("metadata-restore", initErr)
		return nil, initErr
	}
	if err := s.scanTorrentDir(context.Background(), true); err != nil {
		initErr := err
		if closeErr := s.Close(context.Background()); closeErr != nil {
			initErr = errors.Join(err, fmt.Errorf("session: close client after torrent scan: %w", closeErr))
		}
		logInitFailure("torrent-scan", initErr)
		return nil, initErr
	}
	if err := s.loadRegistry(); err != nil {
		initErr := err
		if closeErr := s.Close(context.Background()); closeErr != nil {
			initErr = errors.Join(err, fmt.Errorf("session: close client after state restore: %w", closeErr))
		}
		logInitFailure("state-restore", initErr)
		return nil, initErr
	}
	if err := s.resumeDeletions(); err != nil {
		initErr := err
		if closeErr := s.Close(context.Background()); closeErr != nil {
			initErr = errors.Join(err, fmt.Errorf("session: close client after delete resume: %w", closeErr))
		}
		logInitFailure("delete-resume", initErr)
		return nil, initErr
	}
	if err := s.restorePendingMagnets(context.Background()); err != nil {
		initErr := err
		if closeErr := s.Close(context.Background()); closeErr != nil {
			initErr = errors.Join(err, fmt.Errorf("session: close client after pending magnet restore: %w", closeErr))
		}
		logInitFailure("magnet-restore", initErr)
		return nil, initErr
	}
	scanCtx, scanCancel := context.WithCancel(context.Background())
	s.scanCancel = scanCancel
	s.scanDone = make(chan struct{})
	go s.watchTorrentDir(scanCtx, s.scanDone)
	s.logger.Info("session ready",
		"torrents_dir", s.torrentsDir,
		"data_dir", cfg.Paths.DataDir,
		"cache_capacity_bytes", cfg.Cache.CapacityBytes,
		"listen_port", cfg.Connections.ListenPort,
	)
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
	bgCancel := s.bgCancel
	storageCloser := s.storageCloser
	s.mu.Unlock()
	s.logger.Info("session closing")

	if scanCancel != nil {
		scanCancel()
		if scanDone != nil {
			<-scanDone
		}
	}
	// Deletion and metadata-fetch workers must finish before the torrents are
	// dropped so they never touch a torrent the teardown is closing.
	if bgCancel != nil {
		bgCancel()
	}
	s.bgWg.Wait()

	s.mu.Lock()
	torrents := make([]*Torrent, 0, len(s.torrents))
	for _, t := range s.torrents {
		torrents = append(torrents, t)
	}
	s.mu.Unlock()

	var errs []error
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
	if storageCloser != nil {
		if err := storageCloser.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close default storage: %w", err))
		}
	}

	err := errors.Join(errs...)
	if err == nil {
		s.logger.Info("session closed")
	}
	s.mu.Lock()
	s.torrents = make(map[metainfo.Hash]*Torrent)
	s.metadata = make(map[string]metainfo.Hash)
	s.metadataRefs = make(map[metainfo.Hash]int)
	s.directoryRefs = make(map[metainfo.Hash]int)
	s.directorySource = make(map[string]torrentDirSource)
	s.manualRefs = make(map[metainfo.Hash]struct{})
	s.pendingMagnets = make(map[metainfo.Hash]managedMagnet)
	s.states = make(map[metainfo.Hash]*registryEntry)
	s.operations = make(map[string]*Operation)
	s.activeOps = make(map[metainfo.Hash]string)
	s.lastOps = make(map[metainfo.Hash]string)
	s.metadataFetches = make(map[metainfo.Hash]*metadataFetch)
	s.scanCancel = nil
	s.scanDone = nil
	s.storageCloser = nil
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

// warnLegacyPayloadDir reports an on-disk payload tree left behind by a version
// that persisted pieces. The new session never reads or writes it, so the only
// job here is to tell the operator it can be reclaimed.
func warnLegacyPayloadDir(dataDir string, logger *slog.Logger) {
	root := filepath.Join(dataDir, "payload")
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) == 0 {
		return
	}
	logger.Warn("legacy payload directory is no longer used and can be removed",
		"dir", root,
		"hint", "pieces are now kept in memory only",
	)
}
