package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/sirrobot01/decypharr/internal/state"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/version"
)

// lifecycle is the manager's own readiness state. The task group it runs on
// answers whether work may start; this answers whether the manager is up.
//
//	Constructed → Starting → Ready → Stopping → Stopped
//	                  └─────→ Failed ─┘
//
// Starting, Ready and Failed can all move to Stopping, and so can Constructed
// (a manager that was never started still releases what New opened). Ready
// fires once, on the move from Starting.
type lifecycle uint32

const (
	// lifecycleConstructed is a built manager that Start has not been called on.
	lifecycleConstructed lifecycle = iota + 1
	// lifecycleStarting runs the start sequence; no service is ready yet.
	lifecycleStarting
	// lifecycleReady has completed the start sequence.
	lifecycleReady
	// lifecycleFailed had a start step fail; the group is stopping what it started.
	lifecycleFailed
	// lifecycleStopping is releasing its services, storage last.
	lifecycleStopping
	// lifecycleStopped has closed storage.
	lifecycleStopped
)

func (l lifecycle) String() string {
	switch l {
	case lifecycleConstructed:
		return "constructed"
	case lifecycleStarting:
		return "starting"
	case lifecycleReady:
		return "ready"
	case lifecycleFailed:
		return "failed"
	case lifecycleStopping:
		return stoppingName
	case lifecycleStopped:
		return "stopped"
	default:
		return "lifecycle(" + strconv.FormatUint(uint64(l), 10) + ")"
	}
}

// lifecycleSuccessors is the transition table.
func lifecycleSuccessors(l lifecycle) []lifecycle {
	switch l {
	case lifecycleConstructed:
		return []lifecycle{lifecycleStarting, lifecycleStopping}
	case lifecycleStarting:
		return []lifecycle{lifecycleReady, lifecycleFailed, lifecycleStopping}
	case lifecycleReady, lifecycleFailed:
		return []lifecycle{lifecycleStopping}
	case lifecycleStopping:
		return []lifecycle{lifecycleStopped}
	case lifecycleStopped:
		return nil
	default:
		return nil
	}
}

func newLifecycle() *state.Atomic[lifecycle] {
	return state.NewAtomic(lifecycleConstructed, lifecycleSuccessors)
}

// ErrAlreadyStarted is returned by Start on a manager that was already started.
var ErrAlreadyStarted = errors.New("manager already started")

// service is one sub-service the manager runs as a task on its own child
// group. The task waits for Start to begin it, runs start on the child's
// context, and runs stop when that context ends: the child's place in the
// group's creation order is its place in the shutdown order.
type service struct {
	name  string
	group *task.Group
	begin chan struct{}
	// result carries the outcome of start; buffered so the task never waits on a reader.
	result chan error
}

// superviseService makes the child group name and starts the task that runs
// one sub-service. start may be nil (a service with nothing to start); stop
// runs on every path, including a manager that was never started. A stop
// failure is logged, not returned: the stop of every other service must run.
func (m *Manager) superviseService(
	name string,
	start func(ctx context.Context) error,
	stop func() error,
) (*service, error) {
	child := m.group.Child(name)
	m.children = append(m.children, child)
	svc := &service{name: name, group: child, begin: make(chan struct{}), result: make(chan error, 1)}
	err := child.Go("run", func(ctx context.Context) error {
		select {
		case <-svc.begin:
			svc.result <- m.runStart(ctx, name, start)
		case <-ctx.Done():
		}
		<-ctx.Done()
		if stop != nil {
			if stopErr := stop(); stopErr != nil {
				m.logger.Warn().Err(stopErr).Str("service", name).Msg("Failed to stop service")
			}
		}
		m.logger.Debug().Str("service", name).Msg("Service stopped")
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("supervise %s: %w", name, err)
	}
	return svc, nil
}

// runStart runs a service's start and returns its outcome. A panic is logged
// with its stack and returned as the start's error, so the step that waits
// for the result fails instead of waiting for ever.
func (m *Manager) runStart(ctx context.Context, name string, start func(ctx context.Context) error) (err error) {
	if start == nil {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			m.logger.Error().Str("service", name).Interface("panic", recovered).Bytes("stack", debug.Stack()).
				Msg("Service start panicked")
			err = fmt.Errorf("start %s panicked: %v", name, recovered)
		}
	}()
	return start(ctx)
}

// run starts the service and waits for its start to return.
func (s *service) run(ctx context.Context) error {
	close(s.begin)
	select {
	case err := <-s.result:
		return err
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// Start starts the manager and all its components. It returns once the start
// sequence is under way; Ready fires when it completes. A step that fails
// moves the manager's group to Failed: the services already started stop,
// newest first, and Ready never fires. Start is for one call per manager.
func (m *Manager) Start() error {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	if !m.life.Transition(lifecycleConstructed, lifecycleStarting) {
		switch m.life.Load() {
		case lifecycleStarting, lifecycleReady, lifecycleFailed:
			return ErrAlreadyStarted
		case lifecycleConstructed, lifecycleStopping, lifecycleStopped:
			return task.ErrStopped
		default:
			return task.ErrStopped
		}
	}
	m.startRan = true
	err := m.group.GoCritical("start", func(ctx context.Context) error {
		defer close(m.startDone)
		if startErr := m.startSequence(ctx); startErr != nil {
			if ctx.Err() != nil || errors.Is(startErr, task.ErrStopped) {
				return nil // stopped while starting: the stop is not a failure
			}
			m.life.Transition(lifecycleStarting, lifecycleFailed)
			return startErr
		}
		if !m.life.Transition(lifecycleStarting, lifecycleReady) {
			return nil // stopped while starting
		}
		close(m.ready)
		return nil
	})
	if err != nil {
		close(m.startDone)
	}
	return err
}

// startSequence starts the sub-services in dependency order. Services the
// manager can run without (repair, hearsay) log a failed start and
// the sequence goes on; the Arr services are required.
func (m *Manager) startSequence(ctx context.Context) error {
	m.logger.Info().
		Str("version", version.GetInfo().String()).
		Str("mount_type", string(m.config.Mount.Type)).
		Str("notifications", strconv.FormatBool(m.Notifications.IsEnabled())).
		Str("mount_path", m.config.Mount.MountPath).
		Msg("Starting manager")

	// run the migration process
	m.migrate()

	if err := m.startRequired(ctx, m.reacquireSvc, "Arr reacquisition service"); err != nil {
		return err
	}
	if err := m.startRequired(ctx, m.indexerSvc, "Arr indexer"); err != nil {
		return err
	}

	if err := m.work.Go("initial-sync", m.initialSync); err != nil {
		return fmt.Errorf("start the initial sync: %w", err)
	}
	if err := m.runInitialCalls(); err != nil {
		return fmt.Errorf("start the initial calls: %w", err)
	}

	// Repair registers its sweep on the scheduler, so it starts before the scheduler does.
	m.startOptional(ctx, m.repairSvc, "repair service")
	if err := m.startRequired(ctx, m.schedulerSvc, "manager scheduler"); err != nil {
		return err
	}
	// Start network participation when the operator opted in.
	m.startOptional(ctx, m.hearsaySvc, "hearsay network participation")
	return nil
}

// startRequired starts svc, which a manager cannot run without. A missing
// service (its feature is off) is skipped.
func (m *Manager) startRequired(ctx context.Context, svc *service, what string) error {
	if svc == nil {
		return nil
	}
	if err := svc.run(ctx); err != nil {
		return fmt.Errorf("failed to start %s: %w", what, err)
	}
	return nil
}

// startOptional starts svc and logs a failure.
func (m *Manager) startOptional(ctx context.Context, svc *service, what string) {
	if svc == nil {
		return
	}
	if err := svc.run(ctx); err != nil {
		m.logger.Warn().Err(err).Msgf("Failed to start %s", what)
	}
}

// initialSync brings the library in line with the providers after a start.
func (m *Manager) initialSync(ctx context.Context) error {
	m.syncTorrents(ctx)
	// Sync NZBs
	if err := m.syncNZBs(ctx); err != nil {
		m.logger.Error().Err(err).Msg("Failed to perform initial NZB syncTorrents")
	}
	if fixNZB := os.Getenv("DECYPHARR_FIX_NZB_SIZES"); fixNZB == "1" {
		m.logger.Info().Msg("Starting NZB file size correction as requested by environment variable")
		m.fixNZBFileSizes(ctx)
	}
	// Converge the .strm export tree with config applied since last run.
	m.strm.SweepAsync("startup")
	return nil
}

// Stop stops the manager's group and waits, bounded by ctx: scheduler,
// downloads, repair, Arr services, usenet, hearsay, then storage. The mount is
// not the manager's: main stops it before the manager. An
// owner that holds the group stops it directly; Stop is for one that does not.
func (m *Manager) Stop(ctx context.Context) error {
	m.mountGate.markStopping()
	m.logger.Info().Msg("Stopping manager")
	return m.group.Stop(ctx)
}

// closeStorage is the group's last task: it waits for every child to finish,
// so storage closes after the last download, mount read and repair that could
// touch it, then closes it.
func (m *Manager) closeStorage(ctx context.Context) error {
	<-ctx.Done()
	for _, child := range m.children {
		<-child.Done()
	}
	// The start task is a sibling of this one. A stop during migrate() or the
	// initial calls must not close storage under it. A stop that began before
	// Start was called refuses Start, so there is nothing to wait for.
	m.startMu.Lock()
	ran := m.startRan
	m.startMu.Unlock()
	if ran {
		<-m.startDone
	}
	if m.storage != nil {
		m.logger.Info().Msg("Closing storage database")
		if err := m.storage.Close(); err != nil {
			m.logger.Warn().Err(err).Msg("Failed to close storage")
		}
	}
	m.life.Transition(lifecycleStopping, lifecycleStopped)
	m.logger.Info().Msg("Manager stopped")
	return nil
}

func (m *Manager) markStoppingService() error {
	m.markStopping()
	return nil
}

// markStopping moves the lifecycle to Stopping from wherever it is.
func (m *Manager) markStopping() {
	for {
		current := m.life.Load()
		if current == lifecycleStopping || current == lifecycleStopped {
			return
		}
		if m.life.Transition(current, lifecycleStopping) {
			return
		}
	}
}

// IsReady returns a channel that is closed once the start sequence has
// completed. It is never closed by a manager whose start failed.
func (m *Manager) IsReady() chan struct{} {
	return m.ready
}

// Uptime is how long ago the manager was built.
func (m *Manager) Uptime() time.Duration {
	return time.Since(m.startTime)
}

// StartTime is when the manager was built.
func (m *Manager) StartTime() time.Time {
	return m.startTime
}

// serviceSpec describes one supervised sub-service: where its handle goes
// (nil for a service with nothing to start), how it starts and how it stops.
// The closures read the manager's fields when they run, so the service need
// not exist when it is registered.
type serviceSpec struct {
	name   string
	handle **service
	start  func(ctx context.Context) error
	stop   func() error
}

func (m *Manager) superviseAll(specs []serviceSpec) error {
	for _, spec := range specs {
		svc, err := m.superviseService(spec.name, spec.start, spec.stop)
		if err != nil {
			return err
		}
		if spec.handle != nil {
			*spec.handle = svc
		}
	}
	return nil
}

// registerServices makes the manager's child groups in dependency order: a
// group created earlier stops later, so the services that others rely on
// (usenet, the Arr, repair) outlive the work and the scheduler
// that call them, and hearsay outlives them all. Notification deliveries come
// first so they stop last: every service may call Notify while it stops
// without error (the event is then dropped when the deliveries stop). Storage
// closes in the group's own task, after every child.
func (m *Manager) registerServices() error {
	m.notifyGroup = m.group.Child("notifications")
	m.children = append(m.children, m.notifyGroup)
	if err := m.superviseAll([]serviceSpec{
		{name: "hearsay", handle: &m.hearsaySvc, start: m.startHearsay, stop: m.stopHearsay},
		{name: "usenet", handle: &m.usenetSvc},
		// Provider deletes queued by the services above run here, so they finish after those services have
		// stopped and before usenet and storage go: no stop waits on a slow delete, and no delete is lost.
		{name: "placements", handle: &m.placementsSvc},
		{name: "reacquire", handle: &m.reacquireSvc, start: m.startReacquire, stop: m.stopReacquire},
		{name: "indexer", handle: &m.indexerSvc, start: m.startIndexer, stop: m.stopIndexer},
		{name: "repair", handle: &m.repairSvc, start: m.startRepair, stop: m.stopRepair},
	}); err != nil {
		return err
	}
	// Download actions, the job queue and background provider calls. The mount
	// stops before the whole manager, so they end resumable on the mount gate.
	m.work = m.group.Child("work")
	m.children = append(m.children, m.work)
	return m.superviseAll([]serviceSpec{
		{name: "scheduler", handle: &m.schedulerSvc, start: m.startScheduler, stop: m.stopSchedulerService},
		// The newest child stops first, so its stop marks the manager as stopping before any other service stops.
		{name: "stopping", stop: m.markStoppingService},
	})
}

func (m *Manager) startHearsay(ctx context.Context) error {
	if m.hearsay == nil {
		return nil
	}
	return m.hearsay.Start(ctx)
}

func (m *Manager) stopHearsay() error {
	m.hearsay.Close() // nil-safe
	return nil
}

func (m *Manager) startReacquire(ctx context.Context) error {
	if m.arrService == nil {
		return nil
	}
	return m.arrService.Start(ctx)
}

func (m *Manager) stopReacquire() error {
	defer m.SetArrRecovery(nil)
	if m.arrService == nil {
		return nil
	}
	return m.arrService.Close()
}

func (m *Manager) startIndexer(ctx context.Context) error {
	if m.arrIndexer == nil {
		return nil
	}
	return m.arrIndexer.Start(ctx)
}

func (m *Manager) stopIndexer() error {
	// The queue cleanup search ledger closes first, as it did before the services were tasks.
	if m.arr != nil {
		if err := m.arr.Close(); err != nil {
			m.logger.Warn().Err(err).Msg("Failed to close the queue cleanup search ledger")
		}
	}
	m.arrIndexer.Close() // nil-safe: not configured without the reacquisition service
	return nil
}

func (m *Manager) startRepair(ctx context.Context) error {
	if m.repair == nil {
		return nil
	}
	return m.repair.Start(ctx)
}

func (m *Manager) stopRepair() error {
	if m.repair != nil {
		m.repair.Stop()
	}
	return nil
}

func (m *Manager) stopSchedulerService() error {
	m.stopSchedulers()
	return nil
}
