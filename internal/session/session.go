// Package session owns the BitTorrent client and the torrents registered on
// it. It hides anacrolix behind the small API the rest of torrentfs needs and
// implements vfs.Backend, so main can inject a session straight into
// the filesystem layer.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/yakumioto/torrentfs-go/internal/cache"
	"github.com/yakumioto/torrentfs-go/internal/config"
	"github.com/yakumioto/torrentfs-go/internal/experiments"
	"github.com/yakumioto/torrentfs-go/internal/logging"
	"github.com/yakumioto/torrentfs-go/internal/piecestore"
	"github.com/yakumioto/torrentfs-go/internal/stream"
	"github.com/yakumioto/torrentfs-go/internal/vfs"
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
	// announceRequestDirector, when non-nil, rewrites each HTTP tracker
	// announce request after the client fills in its parameters. Only the
	// experimental synthetic-upload harness sets it; see internal/experiments.
	announceRequestDirector func(*http.Request) error
	// labSettings, when non-nil, enables the session-owned experiment, which
	// supersedes announceRequestDirector when both are set.
	labSettings *experiments.Settings
}

// WithLogger routes session and anacrolix client logs to l.
func WithLogger(l *slog.Logger) Option {
	return func(options *sessionOptions) {
		options.logger = l
	}
}

// WithAnnounceRequestDirector installs a hook that may rewrite each HTTP(S)
// tracker announce request after the client fills in uploaded, downloaded,
// left, event, and the other announce parameters. It exists for the
// experimental synthetic-upload harness (internal/experiments), which fabricates
// announce statistics to exercise tracker-side anomaly detection; a normal
// deployment never sets it. UDP tracker announces and DHT announcements bypass
// the hook.
func WithAnnounceRequestDirector(director func(*http.Request) error) Option {
	return func(options *sessionOptions) {
		options.announceRequestDirector = director
	}
}

// Session owns the anacrolix client and the set of registered torrents.
type Session struct {
	cl *torrent.Client
	// config is the current configuration snapshot. Runtime read paths load it
	// through configSnapshot.load; construction-time consumers keep the
	// validated configuration New was given. See Config and SetConfig.
	config    configSnapshot
	logger    *slog.Logger
	startedAt time.Time
	// streamSigner and streamBaseURL are immutable after startup; a nil signer
	// keeps content streaming off. strm.base_url is restart-only: the signer
	// and every link already handed to a player belong to the startup
	// configuration, so a replacement must not change new links either.
	streamSigner  *stream.Signer
	streamBaseURL string

	pieceCache     *cache.Cache
	pieceStore     *piecestore.Store
	prefetchBudget *prefetchBudget
	mu             sync.RWMutex
	state          lifecycle
	closeDone      chan struct{}
	closeErr       error
	torrents       map[metainfo.Hash]*Torrent
	torrentsDir    string
	metadataDir    string
	subtitleRoot   string
	subtitles      map[metainfo.Hash]map[string]managedSubtitle
	// subtitleStore is the persistent trusted handle for the subtitle tree. It
	// is opened once at startup instead of being re-derived from the path on
	// every operation, so a swap of the store's own path cannot redirect a
	// later read or write.
	subtitleStore *subtitleStore
	// rootNamespaceMu serializes the decisions that depend on the mount root's
	// name layout: a torrent becoming visible and a subtitle being published
	// must not interleave, or a guard decision can be based on a layout that is
	// about to change. It is deliberately separate from mu, which may not be
	// held across a subtitle upload's disk I/O. Acquisition order is always
	// rootNamespaceMu -> per-hash lock -> mu.
	rootNamespaceMu sync.Mutex

	// storageCloser owns the piece store the client does not close on its own
	// when DefaultStorage is set.
	storageCloser storage.ClientImplCloser

	// instanceLock holds the exclusive flock on <torrents-dir>/.metadata so a
	// second process cannot manage the same directory.
	instanceLock *os.File

	// lab is nil unless the session was created with WithLabExperiment. It is
	// guarded by mu; the reporter behind it is safe for concurrent use.
	lab *labState

	// dhtRecorder keeps the last per-family bootstrap outcome for the status
	// API and for transition-only DHT logging.
	dhtRecorder *dhtRecorder

	// stateDir holds the durable per-torrent state sidecars that let the
	// session resume an interrupted deletion after a restart.
	stateDir       string
	categoriesPath string
	categories     map[string]CategoryView

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

	// connAlloc gives every torrent the peer connection allowance its activity
	// calls for. There is no session-wide budget: a torrent that is read gets
	// the reading allowance, a cached one that is seeding keeps a probe slot,
	// and an unused torrent gets no connections at all.
	connAlloc connectionAllocator

	// uploadRate owns the private aggregate upload limiter and the goroutine that
	// switches it at each window boundary or settings update. uploadRateSettings
	// is the normalized sidecar state that the settings API reads back. Both are
	// guarded by mu.
	uploadRate         *uploadRateController
	uploadRateSettings UploadRateSettings

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

// New creates a session and its anacrolix client. The configuration must come
// from the loaders, which are the one place that validates it. The torrents
// directory must already exist; the session creates only its .metadata
// directory. The client is configured to seed, registry-backed tasks are
// restored, and persisted layout checks complete before the session is
// returned.
func New(cfg config.Config, torrentsDir string, opts ...Option) (*Session, error) {
	return newWithClientConfig(cfg, torrentsDir, nil, opts...)
}

func newWithClientConfig(cfg config.Config, torrentsDir string, customize func(*torrent.ClientConfig), opts ...Option) (sess *Session, retErr error) {
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
	subtitleRoot := filepath.Join(metadataDir, "subtitles")
	if err := ensureManagedDirectory(subtitleRoot, 0o700); err != nil {
		err = fmt.Errorf("session: create subtitle dir: %w", err)
		logInitFailure("create-subtitle-dir", err)
		return nil, err
	}
	instanceLock, err := lockInstance(metadataDir)
	if err != nil {
		logInitFailure("acquire-instance-lock", err)
		return nil, err
	}
	// Upload settings are read before anything else is opened: a sidecar that
	// cannot be understood must fail startup rather than start with a limit the
	// operator believes is in force.
	uploadSettings, uploadPolicy, err := loadUploadRateSettings(metadataDir)
	if err != nil {
		releaseInstanceLock(instanceLock)
		logInitFailure("load-upload-rate-settings", err)
		return nil, err
	}
	// The experiment runtime settings are restored before the client is created
	// so the first announce already uses the persisted mode. A sidecar that
	// cannot be understood fails startup, like the upload-rate settings.
	var (
		labRuntime          experiments.RuntimeSettings
		labRuntimePersisted bool
		labReporter         *experiments.Reporter
		labRecorder         *experiments.Recorder
	)
	if options.labSettings != nil {
		labRuntime, labRuntimePersisted, err = loadLabSettings(metadataDir, options.labSettings.RuntimeSettings)
		if err != nil {
			releaseInstanceLock(instanceLock)
			logInitFailure("load-lab-settings", err)
			return nil, err
		}
		if options.labSettings.AnnounceLogPath != "" {
			labRecorder, err = experiments.OpenRecorder(options.labSettings.AnnounceLogPath, logger)
			if err != nil {
				releaseInstanceLock(instanceLock)
				logInitFailure("open-lab-announce-log", err)
				return nil, fmt.Errorf("session: lab experiment: %w", err)
			}
			defer func() {
				if retErr != nil {
					_ = labRecorder.Close()
				}
			}()
			labRecorder.WriteSettings(experiments.Settings{
				RuntimeSettings:    labRuntime,
				Seed:               options.labSettings.Seed,
				AnnounceLogPath:    options.labSettings.AnnounceLogPath,
				PeerSampleInterval: options.labSettings.PeerSampleInterval,
			})
		}
		labReporter = experiments.NewReporter(experiments.Settings{
			RuntimeSettings: labRuntime,
			Seed:            options.labSettings.Seed,
		}, labRecorder)
	}
	var streamSigner *stream.Signer
	if cfg.STRM.BaseURL != "" {
		streamSigner, err = resolveStreamSigner(metadataDir)
		if err != nil {
			releaseInstanceLock(instanceLock)
			logInitFailure("resolve-stream-key", err)
			return nil, err
		}
	}
	// The store handle is opened once here and reused for every later subtitle
	// operation. Anything the session publishes or reads afterwards is resolved
	// from this handle, so replacing the store's path cannot redirect it.
	subtitleStore, err := openSubtitleStore(subtitleRoot)
	if err != nil {
		releaseInstanceLock(instanceLock)
		logInitFailure("open-subtitle-store", err)
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
	// Per-torrent allowances are installed by the connection allocator after
	// start-up; the client keeps its library defaults until then and no
	// session-wide connection cap is configured.
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
	pieceCache := cache.New(cfg.Cache.CapacityBytes.Int64())
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
	// Every session gets a limiter private to itself: the client's own default
	// limiter is a shared pointer, so mutating it would change every other
	// client in the process, and the settings API can enable limiting long after
	// startup. Replacing the pointer later would race the client, so it is
	// installed once, here, with the limit the restored settings put in force
	// now.
	uploadRate := newUploadRateController(uploadPolicy, logger)
	uploadRate.configure(cc)
	if customize != nil {
		customize(cc)
	}
	if options.announceRequestDirector != nil {
		cc.HttpRequestDirector = options.announceRequestDirector
	}
	if labReporter != nil {
		// The session-owned experiment supersedes the raw director option.
		cc.HttpRequestDirector = labReporter.Director
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
	startedAt := time.Now().UTC()
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
		config:          configSnapshot{cfg: cloneConfig(cfg)},
		logger:          logger,
		startedAt:       startedAt,
		streamSigner:    streamSigner,
		streamBaseURL:   cfg.STRM.BaseURL,
		pieceCache:      pieceCache,
		pieceStore:      pieceStore,
		prefetchBudget:  newPrefetchBudget(defaultPrefetchPieces),
		closeDone:       make(chan struct{}),
		torrents:        make(map[metainfo.Hash]*Torrent),
		torrentsDir:     torrentsDir,
		metadataDir:     metadataDir,
		subtitleRoot:    subtitleRoot,
		subtitleStore:   subtitleStore,
		subtitles:       make(map[metainfo.Hash]map[string]managedSubtitle),
		storageCloser:   pieceStore,
		instanceLock:    instanceLock,
		dhtRecorder:     dhtRecorder,
		stateDir:        stateDir,
		categoriesPath:  filepath.Join(metadataDir, "categories.json"),
		categories:      make(map[string]CategoryView),
		states:          make(map[metainfo.Hash]*registryEntry),
		operations:      make(map[string]*Operation),
		activeOps:       make(map[metainfo.Hash]string),
		lastOps:         make(map[metainfo.Hash]string),
		opLocks:         make(map[metainfo.Hash]*sync.Mutex),
		metadataFetches: make(map[metainfo.Hash]*metadataFetch),
		uploadRate:      uploadRate,
		connAlloc:       newConnectionAllocator(),

		uploadRateSettings: uploadSettings,
	}
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	// The scheduler joins the existing background lifecycle, so Close cancels it
	// and waits before the client is torn down.
	s.uploadRate.start(s.bgCtx, &s.bgMu, &s.bgWg)
	s.startConnectionAllocator()
	if labReporter != nil {
		state := &labState{
			settings: *options.labSettings,
			reporter: labReporter,
			recorder: labRecorder,
		}
		s.lab = state
		s.startLabSampler(state)
		logger.Warn("lab upload experiment enabled; only use this build against a tracker you operate",
			"mode", labRuntime.Mode,
			"rate_bytes_per_second", labRuntime.RateBytesPerSecond,
			"multiplier", labRuntime.Multiplier,
			"jitter_percent", labRuntime.JitterFraction*100,
			"cap_bytes", labRuntime.CapBytes,
			"announce_log", options.labSettings.AnnounceLogPath,
			"peer_sample_interval", options.labSettings.PeerSampleInterval.String(),
			"restored_from_sidecar", labRuntimePersisted,
		)
	}
	startup := []struct {
		stage string
		fn    func() error
	}{
		{stage: "category-restore", fn: s.loadCategories},
		{stage: "state-restore", fn: s.loadRegistry},
		{stage: "legacy-migration", fn: s.migrateLegacyLayoutOnce},
		{stage: "delete-resume", fn: s.resumeDeletions},
		{stage: "torrent-restore", fn: func() error {
			return s.restoreRegistryEntries(context.Background())
		}},
		{stage: "subtitle-restore", fn: s.loadManagedSubtitles},
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
		"reading_conns", cfg.Connections.ReadingConns,
		"uploading_conns", cfg.Connections.UploadingConns,
		"read_grace", time.Duration(cfg.Connections.ReadGrace),
		"upload_rate_limit_bytes_per_second", uploadSettings.RateLimitBytesPerSecond,
		"upload_schedule_start", scheduleStartLabel(uploadSettings),
		"upload_schedule_end", scheduleEndLabel(uploadSettings),
		"upload_limited_now", s.uploadLimitedNow(),
	)
	return s, nil
}

// uploadLimitedNow reports whether the session's upload limiter is currently
// limited.
func (s *Session) uploadLimitedNow() bool {
	return s.uploadRate.limitedNow()
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
	subtitleStore := s.subtitleStore
	var labRecorder *experiments.Recorder
	if s.lab != nil {
		labRecorder = s.lab.recorder
	}
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
	// The sampler is already stopped, so the announce log is idle and can be
	// closed after the client has sent its final stopped announces.
	if labRecorder != nil {
		if err := labRecorder.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close lab announce log: %w", err))
		}
	}
	if storageCloser != nil {
		if err := storageCloser.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close default storage: %w", err))
		}
	}
	// The subtitle store handle outlives no work: every subtitle operation runs
	// under mu or the per-hash lock and the client is already closed.
	if err := subtitleStore.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close subtitle store: %w", err))
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
	s.subtitles = make(map[metainfo.Hash]map[string]managedSubtitle)
	s.categories = make(map[string]CategoryView)
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
		return fmt.Errorf("session: not active: %w", vfs.ErrClosed)
	}
	return nil
}
