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
	"strings"
	"sync"

	"github.com/anacrolix/dht/v2"
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

	pieceCache     *cache.Cache
	prefetchBudget *prefetchBudget
	mu             sync.RWMutex
	state          lifecycle
	closeDone      chan struct{}
	closeErr       error
	torrents       map[metainfo.Hash]*Torrent
	torrentsDir    string
	metadataDir    string

	// storageCloser owns the piece store the client does not close on its own
	// when DefaultStorage is set.
	storageCloser storage.ClientImplCloser

	// instanceLock holds the exclusive flock on <torrents-dir>/.metadata so a
	// second process cannot manage the same directory.
	instanceLock *os.File

	// dhtRecorder keeps the last per-family bootstrap outcome for the status
	// API and for transition-only DHT logging.
	dhtRecorder *dhtRecorder

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
	bgMu     sync.Mutex
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
	retry  bool
}

// New creates a session and its anacrolix client. The torrents directory must
// already exist; the session creates only its .metadata directory. The client
// is configured to seed, registry-backed tasks are restored, and persisted
// layout checks complete before the session is returned.
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
	metadataDir := metadataRoot(torrentsDir)
	if err := ensureDirectory(metadataDir); err != nil {
		err = fmt.Errorf("session: create metadata dir: %w", err)
		logInitFailure("create-metadata-dir", err)
		return nil, err
	}
	pendingDir := filepath.Join(metadataDir, "pending")
	if err := ensureDirectory(pendingDir); err != nil {
		err = fmt.Errorf("session: create pending dir: %w", err)
		logInitFailure("create-pending-dir", err)
		return nil, err
	}
	stateDir := filepath.Join(metadataDir, "state")
	if err := ensureDirectory(stateDir); err != nil {
		err = fmt.Errorf("session: create state dir: %w", err)
		logInitFailure("create-state-dir", err)
		return nil, err
	}
	instanceLock, err := lockInstance(metadataDir)
	if err != nil {
		logInitFailure("acquire-instance-lock", err)
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
	cc.ListenHost = func(string) string { return cfg.Connections.ListenHost }
	cc.ListenPort = cfg.Connections.ListenPort
	cc.DisableIPv4 = cfg.Connections.DisableIPv4
	cc.DisableIPv6 = cfg.Connections.DisableIPv6
	cc.NoDefaultPortForwarding = cfg.Connections.NoPortForwarding
	if len(cfg.Connections.BootstrapNodes) > 0 {
		cc.DhtStartingNodes = func(string) dht.StartingNodesGetter {
			return staticStartingNodes(cfg.Connections.BootstrapNodes)
		}
	}
	configureSeeding(cc)
	// Pieces live only in memory: the store keeps them in the LRU cache until
	// they are evicted, and never writes them to disk. Because DefaultStorage is
	// always set, the client's DataDir fallback is unused.
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
	// The peer ID is resolved after customize so an explicitly injected client
	// config always wins, and before NewClient so the durable identity is the
	// one this client announces with.
	peerID, err := resolvePeerID(metadataDir, cc.PeerID, cc.Bep20, logger)
	if err != nil {
		logInitFailure("resolve-peer-id", err)
		releaseInstanceLock(instanceLock)
		return nil, err
	}
	cc.PeerID = peerID
	dhtRecorder := newDhtRecorder()
	configureDhtStartingNodes(cc, dhtRecorder, logger)
	cl, err := torrent.NewClient(cc)
	if err != nil {
		err = fmt.Errorf("session: new client: %w", err)
		logInitFailure("new-client", err)
		releaseInstanceLock(instanceLock)
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
		prefetchBudget:  newPrefetchBudget(defaultPrefetchPieces),
		closeDone:       make(chan struct{}),
		torrents:        make(map[metainfo.Hash]*Torrent),
		torrentsDir:     torrentsDir,
		metadataDir:     metadataDir,
		storageCloser:   pieceStore,
		instanceLock:    instanceLock,
		dhtRecorder:     dhtRecorder,
		stateDir:        stateDir,
		states:          make(map[metainfo.Hash]*registryEntry),
		operations:      make(map[string]*Operation),
		activeOps:       make(map[metainfo.Hash]string),
		lastOps:         make(map[metainfo.Hash]string),
		opLocks:         make(map[metainfo.Hash]*sync.Mutex),
		metadataFetches: make(map[metainfo.Hash]*metadataFetch),
	}
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	startup := []struct {
		stage string
		fn    func() error
	}{
		{stage: "state-restore", fn: s.loadRegistry},
		{stage: "legacy-migration", fn: s.migrateLegacyLayoutOnce},
		{stage: "delete-resume", fn: s.resumeDeletions},
		{stage: "torrent-restore", fn: func() error {
			return s.restoreRegistryEntries(context.Background())
		}},
	}
	for _, step := range startup {
		if err := step.fn(); err != nil {
			initErr := err
			if closeErr := s.Close(context.Background()); closeErr != nil {
				initErr = errors.Join(err, fmt.Errorf("session: close client after %s: %w", step.stage, closeErr))
			}
			logInitFailure(step.stage, initErr)
			return nil, initErr
		}
	}
	s.logger.Info("session ready",
		"torrents_dir", s.torrentsDir,
		"cache_capacity_bytes", cfg.Cache.CapacityBytes,
		"listen_port", cfg.Connections.ListenPort,
		"effective_listen_port", s.EffectiveListenPort(),
		"listen_addrs", strings.Join(s.listenAddrs(), ","),
	)
	return s, nil
}

// EffectiveListenPort returns the port the client actually listens on. With a
// dynamic configured port it is the only place the real value is observable,
// and with a fixed port it is what the tracker announce carries.
func (s *Session) EffectiveListenPort() int {
	return s.cl.LocalPort()
}

func (s *Session) listenAddrs() []string {
	addrs := s.cl.ListenAddrs()
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, addr.String())
	}
	return out
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
	bgCancel := s.bgCancel
	storageCloser := s.storageCloser
	instanceLock := s.instanceLock
	s.mu.Unlock()
	s.logger.Info("session closing")

	// Deletion and metadata-fetch workers must finish before the torrents are
	// dropped so they never touch a torrent the teardown is closing.
	s.bgMu.Lock()
	if bgCancel != nil {
		bgCancel()
	}
	s.bgMu.Unlock()
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
	// cl.Close() shuts the DHT server down, but upstream dht.Server.Close does
	// not join TableMaintainer, so that goroutine can still log one more
	// starting-nodes record through the session logger. stop makes Close
	// return only once no further session log can be written.
	s.dhtRecorder.stop()
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
	// Nothing else touches the torrents directory once the client is closed, so
	// the instance lock is safe to release before the state is finalized.
	releaseInstanceLock(instanceLock)
	if err == nil {
		s.logger.Info("session closed")
	}
	s.mu.Lock()
	s.torrents = make(map[metainfo.Hash]*Torrent)
	s.states = make(map[metainfo.Hash]*registryEntry)
	s.operations = make(map[string]*Operation)
	s.activeOps = make(map[metainfo.Hash]string)
	s.lastOps = make(map[metainfo.Hash]string)
	s.metadataFetches = make(map[metainfo.Hash]*metadataFetch)
	s.storageCloser = nil
	s.instanceLock = nil
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
