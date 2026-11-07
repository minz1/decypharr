package manager

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
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
	work     *task.Group
	children []*task.Group
	// life is the readiness state; ready closes on its move to Ready.
	life  *state.Atomic[lifecycle]
	ready chan struct{}
	// The sub-services the start sequence runs, each as a task on its own
	// child group. A nil service is not configured.
	hearsaySvc, placementsSvc, reacquireSvc, indexerSvc, repairSvc, mountSvc, schedulerSvc *service

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

	// In-flight queue-processor dispatches, keyed by InfoHash, to prevent
	// duplicate goroutines from processing the same entry when the scheduler
	// re-fires before the previous pass has updated the queue row.
	processingEntries *xsync.Map[string, struct{}]

	// Set once restoreActiveDownloadJobs has resubmitted the persisted queue.
	queueRestored atomic.Bool

	// Suppresses repeated provider submissions for the same torrent after an
	// Arr import/re-grab loop. The queue itself handles duplicates while an
	// entry is present; this gate covers the short window after Arr deletes it.
	torrentSubmissions *torrentSubmissionGate

	// Unified active-download queue for torrent and NZB imports.
	jobQueue  *JobQueue
	nzbSyncMu sync.Mutex

	// Notifications service
	Notifications *notifications.Service

	// Hearsay network participation; nil when disabled.
	hearsay *hearsay.Service
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
		group:                  g,
		life:                   newLifecycle(),
		startTime:              time.Now(),
		storage:                strg,
		clients:                xsync.NewMap[string, debrid.Client](),
		logger:                 _logger,
		migrationJobs:          xsync.NewMap[string, *storage.SwitcherJob](),
		config:                 cfg,
		store:                  store,
		logs:                   logs,
		tlsConfig:              tlsConfig,
		fetchClient:            utils.NewDownloadClient(tlsConfig),
		arr:                    arr.New(store, tlsConfig, logs.New("arr")),
		ready:                  make(chan struct{}),
		streamClient:           streamClient,
		usenetTimeout:          usenetTimeout,
		parseRequeueDelay:      defaultParseRequeueDelay,
		badRecheckDelay:        defaultBadRecheckDelay,
		debridSpeedTestResults: xsync.NewMap[string, debridTypes.SpeedTestResult](),
		activeStreams:          xsync.NewMap[string, *ActiveStream](),
		providerMisses:         xsync.NewMap[string, map[string]struct{}](),
		dropHookFailures:       xsync.NewMap[string, map[string]int](),
		processingEntries:      xsync.NewMap[string, struct{}](),
		torrentSubmissions:     newTorrentSubmissionGate(torrentSubmissionDedupWindow),
	}

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
		m.work.Context(),
		m.store,
		m.storage,
		func(ctx context.Context, entry *storage.Entry, filename string) (io.ReadCloser, error) {
			return m.OpenStreamUntracked(ctx, entry, filename, 0)
		},
		m.logger,
	)

	// Set mount paths
	m.setMountPaths()

	m.initEntryCache()
	m.initArrServices()

	// Initialize notifications service
	m.Notifications = notifications.New(&m.config.Notifications, m.tlsConfig, m.logger)

	// Initialize Hearsay state and its default network participation.
	if hs, newErr := hearsay.New(m.config, m.logger); newErr != nil {
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
	m.repair = repair.New(repair.Dependencies{
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
	m.initJobQueue()

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
	service, err := reacquire.NewService(reacquire.ServiceOptions{
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
		m.arr,
		managedArrCatalog{storage: m.storage, logger: m.logger, naming: m.folderNaming},
		service,
		filepath.Join(m.config.Mount.MountPath, EntryAllFolder),
		m.logs.New("arr-indexer"),
	)
	m.SetArrRecovery(service)
}

func (m *Manager) initUsenet() {
	usenetClient, err := usenet.New(m.store, m.logs)
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

func (m *Manager) initJobQueue() {
	m.jobQueue = NewJobQueue(m.work, m.config.MaxActiveDownloads, m.processJob, m.logs.New("jobqueue"))
	// Restore persisted active/queued downloads in the background. With large
	// queues this re-parses thousands of NZBs over the network, and running it
	// inline blocked manager construction — and therefore the HTTP server —
	// for 60-90 minutes on big libraries, during which every arr reported
	// "download client unavailable". Backgrounding lets the API serve and the
	// worker pool drain immediately while the restore catches up.
	entries, err := m.restoreSnapshot()
	if startErr := m.runWork(
		"restore-downloads",
		func() { m.restoreActiveDownloadJobs(entries, err) },
	); startErr != nil {
		m.logger.Warn().Err(startErr).Msg("Active downloads not restored: the manager is stopping")
	}
}

// runWork runs fn as a task of the work group, which the manager's stop
// cancels and waits for. Once the manager stops it starts nothing and
// returns task.ErrStopped: queued entries stay in storage for the next start.
func (m *Manager) runWork(name string, fn func()) error {
	return m.work.Go(name, func(context.Context) error {
		fn()
		return nil
	})
}

func (m *Manager) processJob(ctx context.Context, job *Job) {
	if job == nil {
		return
	}

	var err error
	switch job.Type {
	case JobTypeTorrent:
		err = m.processTorrentJob(ctx, job)
	case JobTypeNZB:
		err = m.processNZBJob(ctx, job)
	default:
		err = fmt.Errorf("unknown job type: %s", job.Type)
	}

	m.passOnUnusedSlot(job, err)
	if err != nil && ctx.Err() == nil {
		m.handleJobError(job, err)
	}
	// The worker slot is released as soon as the job is handed off. Waiting
	// here until the entry left the downloading state parked a worker for the
	// whole post-download lifecycle - including the 30 minute mount wait of the
	// symlink action - so a handful of slow imports stalled every job the arrs
	// submitted. processQueuedEntries drives the entry from here.
}

// throttledRetryDelay doubles the wait after each active-download cap hit, up to maxThrottledRetryDelay.
func throttledRetryDelay(retries int) time.Duration {
	return min(tooManyDownloadsRetryDelay<<min(retries, maxThrottledDoublings), maxThrottledRetryDelay)
}

// failEntry records err on entry and tells its arr, so it sees the failure before the sweep removes it.
func (m *Manager) failEntry(entry *storage.Entry, err error) {
	if removeErr := removeStagedNZB(entry); removeErr != nil {
		m.logger.Warn().Err(removeErr).Str("name", entry.Name).Msg("Failed to remove the failed job's staged NZB")
	} else if entry.IsNZB() {
		entry.Magnet = ""
	}
	entry.MarkAsError(err)
	_ = m.queue.Update(entry)
	if entry.IsNZB() && m.usenet != nil {
		if releaseErr := m.usenet.Fail(entry.InfoHash, err); releaseErr != nil {
			m.logger.Warn().Err(releaseErr).Str("name", entry.Name).Msg("Failed to release the failed NZB")
		}
	}
	m.refreshArr(entry)
}

// failBadRelease fails entry and, for an arr's torrent, its grab: the arr treats qBit "error" as a warning and never re-searches.
func (m *Manager) failBadRelease(entry *storage.Entry, err error) {
	m.failEntry(entry, err)
	grabErr := m.failReleaseGrab(entry)
	switch {
	case grabErr == nil:
	case errors.Is(grabErr, arr.ErrBlocklistHeld):
		// Nothing retries this failure by itself; the held grab is kept and retried while the entry stays in error.
		m.holdGrab(entry.InfoHash, grabBucket(entry), func(context.Context) error {
			if !m.queue.Contains(entry.InfoHash) {
				return nil
			}
			return m.failReleaseGrab(entry)
		})
	default:
		m.logger.Warn().Err(grabErr).Str("name", entry.Name).Msg("Failed to fail the grab in the Arr")
	}
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

// handleJobError drops a job whose entry was deleted, requeues one the provider throttled or whose parse ran out of
// time, or fails its entry.
func (m *Manager) handleJobError(job *Job, err error) {
	// A delete raced the job; failing it would re-create the record.
	if job.Entry != nil && !m.queue.Contains(job.Entry.InfoHash) {
		m.logger.Debug().
			Err(err).
			Str("job_id", job.ID).
			Str("name", job.Entry.Name).
			Msg("Dropped the failed job of a deleted entry")
		return
	}
	if isTooManyActiveDownloads(err) {
		if job.Entry != nil {
			job.Entry.Status = debridTypes.TorrentStatusQueued
			_ = m.queue.Update(job.Entry)
		}
		var providers []string
		if throttled, ok := errors.AsType[*throttledError](err); ok {
			providers = throttled.providers
		}
		delay := throttledRetryDelay(job.ThrottledRetries)
		job.ThrottledRetries++
		m.jobQueue.WaitForSlot(job, providers, delay)
		return
	}
	if errors.Is(err, errParseBudget) {
		if job.ParseRounds+1 < parseRounds {
			m.requeueParse(job, err)
			return
		}
		err = fmt.Errorf("gave up after %d parse rounds: %w", parseRounds, err)
	}
	event := m.logger.Error().Err(err).Str("job_id", job.ID).Str("type", string(job.Type))
	if job.Entry != nil {
		event = event.Str("name", job.Entry.Name)
	}
	event.Msg("Active download failed")
	if job.Entry != nil {
		m.failEntry(job.Entry, err)
	}
}

func (m *Manager) migrate() {
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
	if startErr := m.migrator.Start(); startErr != nil {
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
	if err := m.storage.AddOrUpdate(entry); err != nil {
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
	if deleteErr := m.storage.Delete(infohash); deleteErr != nil {
		return deleteErr
	}
	if removePlacements {
		m.startPlacementRun(torr.Name, "all", func() { m.RemoveTorrentPlacements(torr) })
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

// SubmitJob submits an import to the unified active-download queue.
func (m *Manager) SubmitJob(job *Job) error {
	if m.jobQueue == nil {
		return fmt.Errorf("active download queue not initialized")
	}
	return m.jobQueue.Submit(job)
}

// awaitsJob reports whether a queued entry may still get a job: before the restore, while the job queue holds one,
// or during shutdown (the next start restores it). Holds goes first: shutdown cancels before it releases holds.
func (m *Manager) awaitsJob(infoHash string) bool {
	return !m.queueRestored.Load() || m.jobQueue.Holds(infoHash) || m.work.Context().Err() != nil
}

// slotFreed wakes a job throttled by provider: an active download there finished, failed or was removed.
func (m *Manager) slotFreed(provider string) {
	if m.jobQueue != nil {
		m.jobQueue.SlotFreed(provider)
	}
}

// passOnUnusedSlot wakes the next job waiting on the provider whose freed slot woke job, if job did not take it.
func (m *Manager) passOnUnusedSlot(job *Job, err error) {
	provider := job.wokenBy
	if provider == "" {
		return
	}
	if err == nil && job.DebridTorrent != nil && job.DebridTorrent.Debrid == provider &&
		job.DebridTorrent.Status != debridTypes.TorrentStatusDownloaded {
		return
	}
	if throttled, ok := errors.AsType[*throttledError](err); ok && slices.Contains(throttled.providers, provider) {
		return
	}
	m.slotFreed(provider)
}

func (m *Manager) providerLock(name string) *sync.Mutex {
	v, _ := m.providerLocks.LoadOrStore(name, &sync.Mutex{})
	lock, _ := v.(*sync.Mutex)
	return lock
}
