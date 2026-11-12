package repair

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/hearsay"
	"github.com/sirrobot01/decypharr/pkg/notifications"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet"
)

// Status is the snapshot returned by the /api/repair/status endpoint.
type Status struct {
	Enabled      bool                         `json:"enabled"`
	NextRunAt    *time.Time                   `json:"next_run_at,omitempty"`
	ActiveRun    *storage.RepairRun           `json:"active_run,omitempty"`
	LastRun      *storage.RepairRun           `json:"last_run,omitempty"`
	HealthCounts map[storage.HealthStatus]int `json:"health_counts"`
	// Breaker lists the buckets whose heals are held or being released; empty when every bucket is closed.
	Breaker []BucketStatus `json:"breaker,omitempty"`
}

// RunOptions are one-off options for a manually-started repair run.
// Nil fields inherit the persisted repair config.
type RunOptions struct {
	IgnoreLastChecked bool
	AutoRepair        *bool
	UnrestrictLink    bool
	// VerifyContent additionally head-verifies each NZB media file through
	// the serving stack (container-signature check). Catches files whose
	// articles all exist but were assembled wrong — invisible to the default
	// STAT probe. Costs one article download per file. nil falls back to the
	// configured repair.verify_content setting.
	VerifyContent *bool
	ProtocolScope string
	// MissingProviderIsBroken lets an owner-started recheck act on a provider missing from config; sweeps never do.
	MissingProviderIsBroken bool
}

type ClearStateResult struct {
	Statuses []storage.HealthStatus `json:"statuses"`
	Cleared  int                    `json:"cleared"`
}

const (
	// repairScopeAll is the protocol scope that probes torrents and NZBs.
	repairScopeAll         = "all"
	repairSchedulerTag     = "repair-sweep"
	repairStopSchedulerTag = "repair-sweep-stop"
	repairDefaultWorkers   = 5
	repairDefaultRecheck   = 7 * 24 * time.Hour
	repairHistoryRetained  = 100
	// At most this many files probed concurrently within a single entry. The
	// outer worker count comes from cfg.Repair.Workers.
	repairFilesPerEntry          = 2
	repairStopFinalRepairTimeout = 5 * time.Minute
)

type Backend interface {
	ProviderClient(string) debrid.Client
	ReinsertEntry(context.Context, *storage.Entry) error
	RemoveTorrentFile(string, string) error
	DeleteEntry(string, bool) error
	ClearBad(ctx context.Context, infohash string) error
}

type Reacquirer interface {
	Reacquire(reacquire.Request) (*reacquire.Job, error)
	ReacquireLibraryFile(context.Context, reacquire.LibraryRequest) (*reacquire.Job, error)
}

type Dependencies struct {
	Scheduler     gocron.Scheduler
	Backend       Backend
	Storage       *storage.Storage
	Arrs          *arr.Service
	Reacquirer    Reacquirer
	Usenet        *usenet.Usenet
	Notifications *notifications.Service
	Hearsay       *hearsay.Service
	// Config publishes the repair settings, which are read live. Nil means
	// the zero settings.
	Config *config.Store
	Logger zerolog.Logger
}

// Service checks entry health and coordinates repairs.
type Service struct {
	scheduler     gocron.Scheduler
	backend       Backend
	storage       *storage.Storage
	arrs          *arr.Service
	reacquirer    Reacquirer
	usenet        *usenet.Usenet
	notifications *notifications.Service
	hearsay       *hearsay.Service
	config        *config.Store
	logger        zerolog.Logger

	g *task.Group

	mu             sync.Mutex
	activeRunID    string
	cancelRun      context.CancelFunc
	scheduled      bool
	stopScheduled  bool
	activeStopFunc func()
	// stopped is set by Stop under mu: later runs, rechecks and configuration changes are refused.
	stopped bool

	// onRunExit, when set, is called with a run or recheck ID after its task has released its slot. Tests wait on it.
	onRunExit func(runID string)

	// now is the clock; nil means time.Now. Tests replace it to move through the confirmation gap.
	now     func() time.Time
	breaker *breaker
	// seeded is set once the breaker has been seeded from the journal; guarded by mu.
	seeded bool
	// probeCtx is the context canary probes run on, cancelled by Stop and by ApplyConfig; guarded by mu.
	probeCtx    context.Context
	probeCancel context.CancelFunc
}

// lifetime is the context background probes run on: it ends when the service is stopped or its group ends, and
// ApplyConfig makes a fresh one.
func (r *Service) lifetime() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.probeCtx
}

// exited reports a finished run or recheck to the test hook.
func (r *Service) exited(runID string) {
	if r.onRunExit != nil {
		r.onRunExit(runID)
	}
}

func (r *Service) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// breakerSettings is the live resolved confirmation and breaker configuration.
func (r *Service) breakerSettings() config.BreakerSettings {
	return r.cfg().BreakerSettings()
}

// New builds a repair service from its dependencies.
func New(g *task.Group, deps Dependencies) *Service {
	// A nil *reacquire.Service (its database failed to open) must not become a
	// non-nil Reacquirer, or every auto-repair calls into a nil receiver.
	if rq, ok := deps.Reacquirer.(*reacquire.Service); ok && rq == nil {
		deps.Reacquirer = nil
	}
	r := &Service{
		scheduler:     deps.Scheduler,
		backend:       deps.Backend,
		storage:       deps.Storage,
		arrs:          deps.Arrs,
		reacquirer:    deps.Reacquirer,
		usenet:        deps.Usenet,
		notifications: deps.Notifications,
		hearsay:       deps.Hearsay,
		config:        deps.Config,
		logger:        deps.Logger,
		g:             g,
	}
	r.breaker = newBreaker(r.breakerSettings, r.canaryProbe)
	r.breaker.now = r.clock
	r.breaker.lifetime = r.lifetime
	r.breaker.track = func() (func(), error) { return g.Enter("canary") }
	r.probeCtx, r.probeCancel = context.WithCancel(g.Context())
	return r
}

// folderNaming is the live folder naming scheme.
func (r *Service) folderNaming() config.WebDavFolderNaming {
	if r.config == nil {
		return ""
	}
	return r.config.Get().FolderNaming
}

func (r *Service) cfg() config.RepairConfig {
	if r.config == nil {
		return config.RepairConfig{}
	}
	return r.config.Get().Repair
}

func normalizeRepairProtocolScope(scope string) string {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case repairScopeAll, "both":
		return repairScopeAll
	case string(config.ProtocolTorrent):
		return string(config.ProtocolTorrent)
	case string(config.ProtocolNZB):
		return string(config.ProtocolNZB)
	default:
		return ""
	}
}

func (r *Service) effectiveProtocolScope(opts RunOptions) string {
	if scope := normalizeRepairProtocolScope(opts.ProtocolScope); scope != "" {
		return scope
	}
	return repairScopeAll
}

func repairProtocolMatches(scope string, protocol config.Protocol) bool {
	switch normalizeRepairProtocolScope(scope) {
	case "", repairScopeAll:
		return true
	case string(config.ProtocolTorrent):
		return protocol == config.ProtocolTorrent
	case string(config.ProtocolNZB):
		return protocol == config.ProtocolNZB
	default:
		return true
	}
}

func (r *Service) workers() int {
	if w := r.cfg().Workers; w > 0 {
		return w
	}
	return repairDefaultWorkers
}

func (r *Service) recheckInterval() time.Duration {
	raw := r.cfg().RecheckInterval
	if raw == "" {
		return repairDefaultRecheck
	}
	d, err := utils.ParseDuration(raw)
	if err != nil || d <= 0 {
		return repairDefaultRecheck
	}
	return d
}

// Refused reports how many illegal moves the breaker's buckets refused.
func (r *Service) Refused() uint64 {
	if r == nil || r.breaker == nil {
		return 0
	}
	return r.breaker.refused.Load()
}
