package manager

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"golang.org/x/sync/singleflight"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/state"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/hearsay"
	"github.com/sirrobot01/decypharr/pkg/manager/link"
	"github.com/sirrobot01/decypharr/pkg/manager/virtualfolders"
	"github.com/sirrobot01/decypharr/pkg/notifications"
	"github.com/sirrobot01/decypharr/pkg/repair"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/storage/migration"
	"github.com/sirrobot01/decypharr/pkg/strm"
	"github.com/sirrobot01/decypharr/pkg/usenet"
	"github.com/sirrobot01/decypharr/pkg/usenet/parser"
)

// Streaming HTTP transport tuning and manager defaults.
const (
	streamDialTimeout         = 5 * time.Second
	streamKeepAlive           = 30 * time.Second
	streamTLSSessionCacheSize = 200
	streamTLSHandshakeTimeout = 20 * time.Second
	streamMaxIdleConns        = 1000
	streamMaxConnsPerHost     = 500
	streamIdleConnTimeout     = 120 * time.Second
	streamMaxHeaderBytes      = 1 << 20   // CDN responses can carry large headers
	streamWriteBufferSize     = 32 << 10  // requests are tiny
	streamReadBufferSize      = 256 << 10 // caps how much a single body.Read can return

	defaultUsenetTimeout   = 10 * time.Minute
	defaultRefreshInterval = 15 * time.Minute
	// parseRounds is how many parse rounds that run out of time a grab gets before it fails.
	parseRounds              = 3
	defaultParseRequeueDelay = 5 * time.Minute
	defaultBadRecheckDelay   = 10 * time.Minute
	// tooManyDownloadsRetryDelay is the first wait after the provider reports its active-download limit.
	tooManyDownloadsRetryDelay = 30 * time.Second
	maxThrottledRetryDelay     = 10 * time.Minute
	// maxThrottledDoublings keeps the backoff shift small; 30s doubled 5 times is past the cap.
	maxThrottledDoublings = 5
)

// Manager handles unified torrent management - replaces wire.Store completely.
type Manager struct {
	storage      *storage.Storage
	migrator     *migration.Migrator
	repair       *repair.Service
	clients      *xsync.Map[string, debrid.Client]
	arr          *arr.Service
	arrService   *reacquire.Service
	arrIndexer   *reacquire.Indexer
	logger       zerolog.Logger
	streamClient *http.Client

	// group is the manager's task group, handed in by its owner. Every child
	// below is made from it in dependency order, so stopping the group stops
	// the newest first; storage closes in the group's own last task.
	group *task.Group
	// work owns the manager's background work: download actions, the job
	// queue and fire-and-forget calls to providers, the Arr and the mount.
	work *task.Group
	// strmGroup, a child of work, runs the strm reconciler's background tasks.
	strmGroup *task.Group
	// notifyGroup runs notification deliveries; the first child, it stops last.
	notifyGroup *task.Group
	children    []*task.Group
	// life is the readiness state; ready closes on its move to Ready.
	life *state.Atomic[lifecycle]
	// startMu guards startRan: whether Start launched the start task, whose end startDone announces.
	startMu   sync.Mutex
	startRan  bool
	startDone chan struct{}
	ready     chan struct{}
	// The sub-services the start sequence runs, each as a task on its own
	// child group. A nil service is not configured.
	hearsaySvc, usenetSvc, placementsSvc, reacquireSvc, indexerSvc, repairSvc, schedulerSvc *service

	arrRecoveryMu          sync.RWMutex
	arrRecovery            ArrRecovery
	reacquireNotifications sync.Map
	// heldGrabs are grab failures the arr blocklist guard held back, retried oldest first.
	heldGrabs heldGrabs

	pruneMu      sync.Mutex
	pruneNow     func() time.Time
	removeUsenet func(id string) error
	// checkStreamFile confirms a stream failure; nil uses the usenet client's full article check.
	checkStreamFile func(ctx context.Context, nzoID, filename string) error
	// confirmBroken checks an entry's active placement before a give-up marks it; nil uses the repair service.
	confirmBroken func(ctx context.Context, entry *storage.Entry) bool
	// badRecheckDelay is how long a give-up waits before its second check decides the arr grab.
	badRecheckDelay time.Duration
	// parseNZBFunc parses an NZB's articles; nil uses the usenet client.
	parseNZBFunc  func(ctx context.Context, id, name string, content []byte, category string) (*storage.NZB, map[string]*parser.FileGroup, error)
	pruneReportMu sync.Mutex
	pruneFails    map[string]int
	lastPrune     PruneReport

	// Migration jobs tracking
	migrationJobs   *xsync.Map[string, *storage.SwitcherJob]
	refreshInterval time.Duration

	// config is the snapshot this generation was built from; store
	// publishes the live one, for settings that apply without a restart.
	config *config.Config
	store  *config.Store
	logs   *logger.Factory
	// tlsConfig is the verified TLS base every outgoing client starts from.
	tlsConfig *tls.Config
	// fetchClient downloads NZB and .torrent files from indexers.
	fetchClient *http.Client

	// Processing workers
	scheduler    gocron.Scheduler
	cetScheduler gocron.Scheduler
	queue        *Queue

	// downloading
	refreshSG     singleflight.Group
	providerLocks sync.Map
	linkService   *link.Service

	// repair
	fixer *Fixer

	// strm reconciler
	strm *strm.Reconciler

	virtualFoldersMu sync.RWMutex
	virtualFolders   *virtualfolders.Folders
	mountManager     MountManager
	// mountGate closes when the mount starts to stop; steps that need the mount end resumable once it has.
	mountGate *MountGate
	// stopping ends the moment the manager starts stopping; its cause is task.ErrStopped. grace keeps a ctx alive
	// placementStopGrace past it, for a started provider delete to finish or be cut. Both are made in New; a
	// hand-built test manager calls initGrace to get them.
	stopping    context.Context
	stopCause   context.CancelCauseFunc
	grace       context.Context
	graceCancel context.CancelFunc

	// startTime is when the manager was built; never written afterwards.
	startTime         time.Time
	usenetTimeout     time.Duration
	parseRequeueDelay time.Duration

	rootInfo   *FileInfo
	entry      *EntryCache
	downloader *Downloader
	usenet     *usenet.Usenet

	// Debrid speed test results storage
	debridSpeedTestResults *xsync.Map[string, debridTypes.SpeedTestResult]

	// Active streams tracking
	activeStreams *xsync.Map[string, *ActiveStream]

	// providerMisses holds each provider's previous-sync misses (infohashes).
	providerMisses *xsync.Map[string, map[string]struct{}]
	// dropHookFailures counts failed Arr recoveries per provider and infohash.
	dropHookFailures *xsync.Map[string, map[string]int]

	nzbSyncMu sync.Mutex

	// grabs is the read index of every grab; entries is the actor that owns them.
	grabs   *grabIndex
	entries *entryActor

	// Notifications service
	Notifications *notifications.Service

	// Hearsay network participation; nil when disabled.
	hearsay *hearsay.Service
}

// placementStopGrace is how long a started provider delete may run after the
// manager starts stopping. A delete still going when it ends is cut.
const placementStopGrace = 5 * time.Second

// initGrace makes the manager's stopping and grace contexts from parent's
// values, never its cancel: only markStopping ends stopping. New always calls
// it; a hand-built test manager calls it too when it must exercise the stop
// grace. stopping is cancelled with task.ErrStopped the moment the manager
// starts stopping; grace keeps a ctx alive placementStopGrace past that.
func (m *Manager) initGrace(parent context.Context) {
	if m.stopping != nil {
		return
	}
	m.stopping, m.stopCause = context.WithCancelCause(context.WithoutCancel(parent))
	m.grace, m.graceCancel = task.Grace(m.stopping, placementStopGrace)
}

var _ repair.Backend = (*Manager)(nil)

// New builds the manager for one service generation from the configuration
// published by store. g is the manager's task group: its owner made it with
// parent.Child("manager") and stops it, which stops the manager's services
// and closes what New opened. A restart builds a new Manager from a new Store.
func New(g *task.Group, store *config.Store, logs *logger.Factory) (*Manager, error) {
	cfg := store.Get()
	_logger := logs.New("manager")

	// Verified TLS for every outgoing client: debrid APIs and CDNs, *arr.
	tlsConfig, err := cfg.TLSClientConfig()
	if err != nil {
		return nil, err
	}

	strg, err := storage.NewStorage(filepath.Join(cfg.Dir(), "db"), storage.Options{
		FolderNaming: func() config.WebDavFolderNaming { return store.Get().FolderNaming },
		Logger:       logs.New("storage"),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create manager storage: %w", err)
	}

	// Optimized transport for high-performance streaming with HTTP/2 multiplexing
	// DNS resolver with caching
	dialer := &net.Dialer{
		Timeout:   streamDialTimeout,
		KeepAlive: streamKeepAlive,
	}

	streamTLS := tlsConfig.Clone()
	streamTLS.ClientSessionCache = tls.NewLRUClientSessionCache(streamTLSSessionCacheSize)
	transport := &http.Transport{
		TLSClientConfig:        streamTLS,
		TLSHandshakeTimeout:    streamTLSHandshakeTimeout,
		MaxIdleConns:           streamMaxIdleConns,
		MaxIdleConnsPerHost:    streamMaxConnsPerHost,
		MaxConnsPerHost:        streamMaxConnsPerHost,
		IdleConnTimeout:        streamIdleConnTimeout,
		DisableCompression:     false, // Enable compression for better multiplexing
		DialContext:            dialer.DialContext,
		Proxy:                  http.ProxyFromEnvironment,
		MaxResponseHeaderBytes: streamMaxHeaderBytes,
		WriteBufferSize:        streamWriteBufferSize,
		ReadBufferSize:         streamReadBufferSize,
	}

	streamClient := &http.Client{
		Timeout:   0,
		Transport: transport,
	}

	usenetTimeout, err := utils.ParseDuration(cfg.Usenet.ProcessingTimeout)
	if err != nil {
		usenetTimeout = defaultUsenetTimeout
	}

	instance := &Manager{
		group:         g,
		life:          newLifecycle(),
		mountGate:     NewMountGate(_logger),
		startDone:     make(chan struct{}),
		startTime:     time.Now(),
		storage:       strg,
		clients:       xsync.NewMap[string, debrid.Client](),
		logger:        _logger,
		migrationJobs: xsync.NewMap[string, *storage.SwitcherJob](),
		config:        cfg,
		store:         store,
		logs:          logs,
		tlsConfig:     tlsConfig,
		fetchClient:   utils.NewDownloadClient(tlsConfig),
		// The Arr service's background work, such as a canary probe, ends with the manager's group.
		arr:                    arr.New(store, tlsConfig, logs.New("arr"), arr.WithLifetime(g.Context())),
		ready:                  make(chan struct{}),
		streamClient:           streamClient,
		usenetTimeout:          usenetTimeout,
		parseRequeueDelay:      defaultParseRequeueDelay,
		badRecheckDelay:        defaultBadRecheckDelay,
		debridSpeedTestResults: xsync.NewMap[string, debridTypes.SpeedTestResult](),
		activeStreams:          xsync.NewMap[string, *ActiveStream](),
		providerMisses:         xsync.NewMap[string, map[string]struct{}](),
		dropHookFailures:       xsync.NewMap[string, map[string]int](),
	}
	instance.initGrace(g.Context())

	if err = instance.init(); err != nil {
		// The group may be stopped already; storage is the one thing New opened that nothing else closes.
		if closeErr := strg.Close(); closeErr != nil {
			_logger.Warn().Err(closeErr).Msg("Failed to close storage after a failed start")
		}
		return nil, err
	}
	return instance, nil
}

// folderNaming is the live folder naming scheme.
func (m *Manager) folderNaming() config.WebDavFolderNaming {
	return m.store.Get().FolderNaming
}

func (m *Manager) init() error {
	if err := m.registerServices(); err != nil {
		return err
	}
	cfg := m.config
	scheduler, err := gocron.NewScheduler(
		gocron.WithLocation(time.Local),
		gocron.WithGlobalJobOptions(gocron.WithTags("decypharr-manager")),
	)
	if err != nil {
		scheduler, _ = gocron.NewScheduler(gocron.WithGlobalJobOptions(gocron.WithTags("decypharr-manager")))
	}

	// Create CET scheduler for time-specific jobs
	cetLocation, err := time.LoadLocation("CET")
	if err != nil {
		cetLocation = time.UTC
	}
	cetScheduler, err := gocron.NewScheduler(
		gocron.WithLocation(cetLocation),
		gocron.WithGlobalJobOptions(gocron.WithTags("decypharr-cet")),
	)
	if err != nil {
		cetScheduler, _ = gocron.NewScheduler(gocron.WithGlobalJobOptions(gocron.WithTags("decypharr-cet")))
	}

	m.queue = newQueue(m.storage, cfg.RemoveStalledAfter, m.folderNaming, m.logs.New("queue"))

	m.scheduler = scheduler
	m.cetScheduler = cetScheduler
	m.migrator = migration.New(m.storage, cfg.Dir(), m.logs.New("migrator"))
	m.downloader = NewDownloadManager(m)

	// Initialize HTTP pool for streaming
	// Note: We can't create a single pool for all files because the LinkRefresh callback
	// needs torrent+filename context. Instead, manager.Stream will create a pool per request
	// and cache it. This is actually better because different files may have different
	// download links from different CDNs.

	refreshInterval, err := utils.ParseDuration(cfg.RefreshInterval)
	if err != nil {
		refreshInterval = defaultRefreshInterval
	}
	m.refreshInterval = refreshInterval

	// initialize debrid clients
	m.initDebridClients()

	// Initialize usenet client
	m.initUsenet()

	// Initialize link service
	m.initLinkService()

	// Initialize virtual folders.
	m.initVirtualFolders()

	// Initialize fixer
	m.fixer = NewFixer(m)

	// Initialize strm reconciler
	m.strm = strm.NewReconciler(
		m.strmGroup,
		m.store,
		m.storage,
		func(ctx context.Context, entry *storage.Entry, filename string) (io.ReadCloser, error) {
			return m.OpenStreamUntracked(ctx, entry, filename, 0)
		},
		m.putLibrary,
		m.logger,
	)

	// Set mount paths
	m.setMountPaths()

	m.initEntryCache()
	m.initArrServices()

	// Initialize notifications service
	m.Notifications = notifications.New(m.notifyGroup, &m.config.Notifications, m.tlsConfig, m.logger)

	// Initialize Hearsay state and its default network participation.
	if hs, newErr := hearsay.New(m.hearsaySvc.group, m.config, m.logger); newErr != nil {
		m.logger.Warn().Err(newErr).Msg("Hearsay disabled: failed to initialize")
	} else {
		m.hearsay = hs
	}

	// A nil *reacquire.Service must reach repair as a nil interface, not a
	// non-nil Reacquirer wrapping a nil pointer.
	var reacquirer repair.Reacquirer
	if m.arrService != nil {
		reacquirer = m.arrService
	}
	m.repair = repair.New(m.repairSvc.group, repair.Dependencies{
		Scheduler:     m.scheduler,
		Backend:       m,
		Storage:       m.storage,
		Arrs:          m.arr,
		Reacquirer:    reacquirer,
		Usenet:        m.usenet,
		Notifications: m.Notifications,
		Hearsay:       m.hearsay,
		Config:        m.store,
		Logger:        m.logs.New("repair"),
	})
	m.arr.SetQueueGuardHooks(m.queueGuardHooks())

	// Initialize the unified active-download queue after all processors exist.
	records, packOf := m.readQueue()
	m.initEntries(packOf)
	m.recoverEntries(records, packOf)

	// Storage closes last: this task ends when the group's own context does,
	// which is after every child group has stopped.
	if err = m.group.Go("storage", m.closeStorage); err != nil {
		return fmt.Errorf("own the manager's storage: %w", err)
	}
	return nil
}

func (m *Manager) initArrServices() {
	// Queue cleanup keeps the searches it owes in a file of its own; without it they live in memory only.
	ledgerPath := filepath.Join(m.config.Dir(), "db", queueCleanupLedgerName)
	if err := m.arr.OpenSearchLedger(ledgerPath); err != nil {
		m.logger.Error().Err(err).
			Msg("Failed to open the queue cleanup search ledger; owed searches will not survive a restart")
	}
	service, err := reacquire.NewService(m.reacquireSvc.group, reacquire.ServiceOptions{
		Arrs:      m.arr,
		Directory: filepath.Join(m.config.Dir(), "db"),
		Handler:   reacquire.NewHandler(m.arr, m),
		Logger:    m.logger,

		OnNeedsAttention: m.announceReacquireAttention,
	})
	if err != nil {
		m.logger.Error().Err(err).Msg("Failed to initialize Arr reacquisition service")
		m.arrService = nil
		m.arrIndexer = nil
		m.SetArrRecovery(nil)
		return
	}
	m.arrService = service
	m.arrIndexer = reacquire.NewIndexer(
		m.indexerSvc.group,
		m.arr,
		managedArrCatalog{storage: m.storage, write: m.putLibrary, logger: m.logger, naming: m.folderNaming},
		service,
		filepath.Join(m.config.Mount.MountPath, EntryAllFolder),
		m.logs.New("arr-indexer"),
	)
	m.SetArrRecovery(service)
}

func (m *Manager) initUsenet() {
	usenetClient, err := usenet.New(m.usenetSvc.group, m.store, m.logs)
	if err != nil {
		m.logger.Warn().Msg("Usenet client not configured")
		m.usenet = nil
		return
	}
	m.usenet = usenetClient
}

// initLinkService initializes the link service.
func (m *Manager) initLinkService() {
	m.linkService = link.New(
		m.clients,
		m.refreshTorrent,
		m.ReinsertEntry,
		m.giveUpOnEntry,
		m.streamClient,
		m.config.Retries,
		m.folderNaming,
		m.logs.New("link"),
	)
}

func (m *Manager) initEntries(packOf map[string]string) {
	packs := newPackIndex(packOf)
	m.grabs = &grabIndex{}
	m.entries = newEntryActor(m.work, entryDeps{
		store:     m.storage,
		grabs:     m.grabs,
		logger:    m.logs.New("grab"),
		capacity:  m.config.MaxActiveDownloads,
		child:     m.entryChild,
		publish:   m.storage.PublishEntry,
		fail:      m.failLater,
		packOf:    packs.snapshot,
		packAdd:   packs.add,
		seasonsOf: m.seasonEntries,
		naming:    m.folderNaming,
		inLibrary: func(key string) bool {
			ok, err := m.storage.Exists(key)
			return ok && err == nil
		},
		pollEvery:  m.refreshInterval,
		retryDelay: throttledRetryDelay,
		completed:  m.completed,
	})
}

// runWork runs fn as a task of the work group, which the manager's stop
// cancels and waits for. Once the manager stops it starts nothing and
// returns task.ErrStopped: queued entries stay in storage for the next start.
func (m *Manager) runWork(name string, fn func()) error {
	return m.runWorkCtx(name, func(context.Context) { fn() })
}

// runWorkCtx is runWork for a step that calls providers on the work task's ctx.
func (m *Manager) runWorkCtx(name string, fn func(ctx context.Context)) error {
	return m.work.Go(name, func(ctx context.Context) error {
		fn(ctx)
		return nil
	})
}

// throttledRetryDelay doubles the wait after each active-download cap hit, up to maxThrottledRetryDelay.
func throttledRetryDelay(retries int) time.Duration {
	return min(tooManyDownloadsRetryDelay<<min(retries, maxThrottledDoublings), maxThrottledRetryDelay)
}

// failReleaseGrab fails the grab of an arr's torrent whose release the provider rejected.
func (m *Manager) failReleaseGrab(entry *storage.Entry) error {
	if !entry.IsTorrent() || entry.Manual {
		return nil
	}
	instance, ok := m.Arr().Get(entry.Category)
	recovery := m.recoveryService()
	if !ok || recovery == nil {
		return nil
	}
	return m.failGrab(entry, recovery, instance.Name, "Provider rejected the torrent")
}

func (m *Manager) migrate(ctx context.Context) {
	// Check if migration has already been done
	status, err := m.migrator.GetStatus()
	if err == nil && !status.Running && status.Completed > 0 {
		m.logger.Info().
			Int("completed", status.Completed).
			Msg("Migration already completed previously")
		return
	}

	// GetReader migration stats to see if there are cache files
	stats, err := m.migrator.GetStats()
	if err != nil {
		m.logger.Warn().Err(err).Msg("Failed to get migration stats")
		return
	}

	cacheFiles, ok := stats["cache_files"].(int)
	if !ok || cacheFiles == 0 {
		return
	}

	cacheTorrents, ok := stats["cache_torrents"].(int)
	if !ok {
		cacheTorrents = 0
	}

	m.logger.Info().
		Int("cache_files", cacheFiles).
		Int("unique_torrents", cacheTorrents).
		Msg("Found cache files, starting automatic migration...")

	// Start migration with backup
	if startErr := m.migrator.Start(ctx); startErr != nil {
		m.logger.Error().Err(startErr).Msg("Failed to start automatic migration")
		return
	}

	m.logger.Info().Msg("Automatic migration started successfully")
}

func (m *Manager) stopSchedulers() {
	if m.scheduler != nil {
		if err := m.scheduler.Shutdown(); err != nil {
			m.logger.Warn().Err(err).Msg("Failed to shutdown scheduler")
		}
	}
	if m.cetScheduler != nil {
		if err := m.cetScheduler.Shutdown(); err != nil {
			m.logger.Warn().Err(err).Msg("Failed to shutdown CET scheduler")
		}
	}
}

// queueCleanupLedgerName is the file, under the db directory, that holds the searches queue cleanup owes.
const queueCleanupLedgerName = "queue_cleanup_searches.db"

func (m *Manager) GetStats() (map[string]any, error) {
	count, err := m.storage.Count()
	if err != nil {
		return nil, err
	}

	diskSize := m.storage.DiskSize()
	activeJobs := 0
	completedJobs := 0
	failedJobs := 0
	m.migrationJobs.Range(func(_ string, job *storage.SwitcherJob) bool {
		switch job.Status {
		case storage.SwitcherStatusPending, storage.SwitcherStatusInProgress:
			activeJobs++
		case storage.SwitcherStatusCompleted:
			completedJobs++
		case storage.SwitcherStatusFailed, storage.SwitcherStatusCancelled:
			failedJobs++
		}
		return true
	})

	return map[string]any{
		"total_torrents": count,
		"storage_stats":  map[string]any{"total_size": diskSize},
		"active_jobs":    activeJobs,
		"completed_jobs": completedJobs,
		"failed_jobs":    failedJobs,
	}, nil
}

// RefusalReporter is a mount that reports, by state machine, the moves it refused.
type RefusalReporter interface {
	Refusals() map[string]uint64
}

// Refusals reports, by state machine, how many illegal moves the manager's
// machines and its mount's refused.
func (m *Manager) Refusals() map[string]uint64 {
	out := map[string]uint64{
		"manager":         m.life.Refused(),
		"mount_gate":      m.mountGate.Refused(),
		"repair_breaker":  m.repair.Refused(),
		"arr_queue_guard": m.arr.Refused(),
	}
	if m.Notifications != nil {
		out["notifications"] = m.Notifications.Refused()
	}
	if m.hearsay != nil {
		out["hearsay"] = m.hearsay.Refused()
	}
	if mm, ok := m.mountManager.(RefusalReporter); ok {
		maps.Copy(out, mm.Refusals())
	}
	return out
}

// CRUD operations

func (m *Manager) GetEntryItem(torrentName string) (*storage.EntryItem, error) {
	return m.storage.GetEntryItem(torrentName)
}

func (m *Manager) GetEntryByName(torrentName, filename string) (*storage.Entry, error) {
	// First get entry
	entry, err := m.storage.GetEntryItem(torrentName)
	if err != nil {
		return nil, err
	}

	// Find the file in the entry
	file, err := entry.GetFile(filename)
	if err != nil {
		return nil, err
	}
	return m.GetEntry(file.InfoHash)
}

func (m *Manager) AddOrUpdate(entry *storage.Entry, callback func(t *storage.Entry)) error {
	entry.UpdatedAt = time.Now()
	if err := m.writeLibrary(m.work.Context(), entry.InfoHash, libraryOp{kind: libPut, entry: entry}); err != nil {
		return err
	}
	// Keep .strm files derived state: any post-completion update (repair,
	// refresh, provider switch) re-syncs them. Cheap and idempotent.
	if entry.IsComplete {
		m.strm.SyncEntryAsync(entry)
	}
	if callback != nil {
		// The callback only refreshes derived views; a stopping manager drops it.
		if err := m.runWork("add-or-update-callback", func() { callback(entry) }); err != nil {
			m.logger.Debug().Err(err).Str("infohash", entry.InfoHash).Msg("Dropped the save callback")
		}
	}
	return nil
}

// GetEntry gets a torrent by name.
func (m *Manager) GetEntry(infohash string) (*storage.Entry, error) {
	return m.storage.Get(infohash)
}

func (m *Manager) EntryExists(infohash string) (bool, error) {
	return m.storage.Exists(infohash)
}

func (m *Manager) GetTorrents(filter func(*storage.Entry) bool) ([]*storage.Entry, error) {
	// Use streaming to avoid loading all torrents into memory at once
	var torrents []*storage.Entry
	err := m.storage.ForEach(func(t *storage.Entry) error {
		if filter == nil || filter(t) {
			torrents = append(torrents, t)
		}
		return nil
	})
	return torrents, err
}

func (m *Manager) GetTorrentsCount() (int, error) {
	return m.storage.Count()
}

// DeleteEntry deletes a torrent by infohash.
func (m *Manager) DeleteEntry(infohash string, removePlacements bool) error {
	torr, err := m.GetEntry(infohash)
	if err != nil {
		return err
	}
	// The record goes first: a failed delete leaves the entry, and its provider torrent must stay with it. Once it
	// is gone the entry no longer counts among the torrent's holders.
	op := libraryOp{kind: libDelete, cascade: true}
	if deleteErr := m.writeLibrary(m.work.Context(), infohash, op); deleteErr != nil {
		return deleteErr
	}
	if removePlacements {
		m.startPlacementRun(torr.Name, "all", func(context.Context) { m.RemoveTorrentPlacements(torr) })
	}
	m.strm.RemoveEntryAsync(torr)
	// Refresh entry cache
	m.InvalidateEntryCache()
	if workErr := m.runWork("refresh-mount", func() {
		if refreshMountErr := m.RefreshMount(); refreshMountErr != nil {
			m.logger.Error().Err(refreshMountErr).Msg("Mount refresh after entry deletion failed")
		}
	}); workErr != nil {
		m.logger.Debug().Err(workErr).Msg("Skipped the mount refresh after entry deletion")
	}
	return nil
}

func (m *Manager) DeleteTorrents(infohashes []string, removeFromDebrid bool) error {
	for _, infohash := range infohashes {
		if err := m.DeleteEntry(infohash, removeFromDebrid); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) GetMigrationJob(jobID string) (*storage.SwitcherJob, error) {
	job, exists := m.migrationJobs.Load(jobID)
	if !exists {
		return nil, fmt.Errorf("migration job not found: %s", jobID)
	}
	return job, nil
}

// awaitsJob reports whether a queued entry is still in its grab's hands, or the manager is stopping (the next start
// recovers it).
func (m *Manager) awaitsJob(infoHash string) bool {
	return m.grabs.active(infoHash) || m.stopErr() != nil
}

// slotFreed wakes the grabs throttled by provider: an active download there finished, failed or was removed.
func (m *Manager) slotFreed(provider string) {
	if m.entries != nil {
		m.entries.admit.SlotFreed(provider)
	}
}

func (m *Manager) providerLock(name string) *sync.Mutex {
	v, _ := m.providerLocks.LoadOrStore(name, &sync.Mutex{})
	lock, _ := v.(*sync.Mutex)
	return lock
}
