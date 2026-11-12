package dfs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/state"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/backend"
	fuseconfig "github.com/sirrobot01/decypharr/pkg/mount/dfs/config"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/vfs"
)

// Manager manages FUSE filesystem instances with proper caching.
//
// Two task groups hang off the service group, in this order: "vfs" (the
// cache) and "backend" (the owner task). The newer child stops first, so the
// owner unmounts before the vfs closer closes the cache.
type Manager struct {
	manager            *manager.Manager
	logger             zerolog.Logger
	state              *state.Atomic[dfsState]
	defaultBackendType backend.Type
	config             *fuseconfig.FuseConfig
	logs               *logger.Factory

	// newBackend and closeVFS are test seams.
	newBackend func(*vfs.Manager) (backend.Backend, error)
	closeVFS   func() error

	// mu guards the state move and group setup in Start, and Stop's read of
	// them. It is not held while waiting for the owner.
	mu           sync.Mutex
	vfsGroup     *task.Group
	backendGroup *task.Group
	vfs          *vfs.Manager
	// stopSlice is the generation's mount slice; the stop waits end with it once it began.
	stopSlice atomic.Pointer[task.Slice]
	backend   backend.Backend
	// failure is why the mount last moved to Failed; Stats shows it while Failed.
	failure atomic.Pointer[string]
}

// NewManager creates a new FUSE filesystem manager for the DFS settings in cfg.
func NewManager(manager *manager.Manager, cfg *config.Config, logs *logger.Factory) *Manager {
	fuseConfig := fuseconfig.Parse(cfg.Mount.DFS, cfg.Mount.MountPath, cfg.Retries)
	fuseConfig.MountDirMode = cfg.SharedDirModeValue()

	m := &Manager{
		manager:            manager,
		logs:               logs,
		logger:             logs.New("dfs"),
		state:              state.NewAtomic(dfsNew, dfsSuccessors),
		defaultBackendType: backend.GetDefaultBackendType(),
		config:             fuseConfig,
	}
	m.newBackend = func(v *vfs.Manager) (backend.Backend, error) {
		return newBackend(m.defaultBackendType, v, m.config, m.logs)
	}
	m.closeVFS = func() error { return m.vfs.Close() }
	return m
}

// move changes the state from one value to another. An illegal edge is a bug
// and logs an error; a legal edge lost to a concurrent mover is expected.
func (m *Manager) move(from, to dfsState) bool {
	if m.state.Transition(from, to) {
		m.logger.Debug().Stringer("from", from).Stringer("to", to).Msg("dfs state moved")
		return true
	}
	if !state.Legal(dfsSuccessors, from, to) {
		m.logger.Error().Stringer("from", from).Stringer("to", to).Msg("dfs state move refused")
		return false
	}
	m.logger.Debug().Stringer("from", from).Stringer("to", to).Msg("dfs state move lost")
	return false
}

// advance moves to the state from the first of froms the machine is in, and
// does nothing when it is in none of them.
func (m *Manager) advance(froms []dfsState, to dfsState) {
	for _, from := range froms {
		if m.state.Transition(from, to) {
			m.logger.Debug().Stringer("from", from).Stringer("to", to).Msg("dfs state moved")
			return
		}
	}
}

// failWith moves to Failed from the first of froms the machine is in, keeping cause for Stats; it reports whether
// it moved. The cause is stored before the state moves, so a concurrent Stats reader that sees Failed already has the
// error to show.
func (m *Manager) failWith(cause string, froms ...dfsState) bool {
	for _, from := range froms {
		m.failure.Store(&cause)
		if m.state.Transition(from, dfsFailed) {
			m.logger.Debug().Stringer("from", from).Stringer("to", dfsFailed).Msg("dfs state moved")
			return true
		}
	}
	return false
}

// SetStopSlice gives the manager the generation's mount slice, before Start.
// The cache gets it too, so its close ends with the slice as well.
func (m *Manager) SetStopSlice(s task.Slice) {
	if s == nil {
		m.stopSlice.Store(nil)
		return
	}
	m.stopSlice.Store(&s)
}

// slice is the manager's stop slice; the zero Slice (unarmed) when none was set.
func (m *Manager) slice() task.Slice {
	if s := m.stopSlice.Load(); s != nil {
		return *s
	}
	return nil
}

// Start mounts the filesystem and returns once it is ready (or failed).
func (m *Manager) Start(g *task.Group) error {
	m.mu.Lock()
	cur := m.state.Load()
	if cur == dfsStopping || cur == dfsStopped {
		m.mu.Unlock()
		return task.ErrStopped
	}
	if cur != dfsNew || !m.move(dfsNew, dfsStarting) {
		m.mu.Unlock()
		return errStarted
	}

	m.logger.Info().
		Str("mount_path", m.config.MountPath).
		Str("backend", string(m.defaultBackendType)).
		Msg("Starting DFS with backend")

	// Both children first: the child order is the stop order.
	vfsG := g.Child("vfs")
	backG := g.Child("backend")
	m.vfsGroup, m.backendGroup = vfsG, backG

	vfsManager, err := vfs.NewManager(vfsG, m.manager, m.config, m.logs)
	if err != nil {
		err = fmt.Errorf("failed to create VFS manager: %w", err)
		m.failWith(err.Error(), dfsStarting)
		backG.Close()
		vfsG.Close()
		m.mu.Unlock()
		return err
	}
	vfsManager.SetStopSlice(m.slice())
	m.vfs = vfsManager
	bck, err := m.newBackend(vfsManager)
	if err != nil {
		// Nothing will ever mount: release the cache now.
		err = fmt.Errorf("failed to start DFS backend: %w", err)
		m.failWith(err.Error(), dfsStarting)
		m.releaseVFS()
		backG.Close()
		vfsG.Close()
		m.mu.Unlock()
		return err
	}
	m.backend = bck

	res := newStartResult()
	if err = vfsG.Go("closer", m.closer(vfsG, backG)); err != nil {
		m.abortStart(vfsG, backG)
		m.mu.Unlock()
		return err
	}
	if err = backG.GoCritical("owner", m.owner(bck, backG, res)); err != nil {
		backG.Close()
		m.mu.Unlock()
		return task.ErrStopped
	}
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(g.Context(), m.config.DaemonTimeout)
	defer cancel()
	select {
	case <-res.done:
		if res.err != nil {
			backG.Close()
			return res.err
		}
		m.logger.Info().
			Str("mount_path", m.config.MountPath).
			Str("backend", string(m.defaultBackendType)).
			Stringer("state", m.state.Load()).
			Msg("DFS started successfully")
		return nil
	case <-ctx.Done():
		if g.Context().Err() != nil {
			backG.Close()
			return task.ErrStopped
		}
		// If the owner wins Ready just as DaemonTimeout fires, this move loses and
		// the state ends Stopped, not Failed (P3).
		err = fmt.Errorf("DFS mount timed out after %s: %w", m.config.DaemonTimeout, ctx.Err())
		m.failWith(err.Error(), dfsStarting)
		backG.Close()
		return err
	}
}

// abortStart releases a start whose groups refused their tasks (the parent is
// stopping).
func (m *Manager) abortStart(vfsG, backG *task.Group) {
	backG.Close()
	m.advance([]dfsState{dfsStarting}, dfsStopping)
	m.releaseVFS()
	m.advance([]dfsState{dfsStopping}, dfsStopped)
	vfsG.Close()
}

// Stop stops the groups, backend first, on one budget. Under the production
// parent-group stop they have ended already and this returns at once.
func (m *Manager) Stop() error {
	m.mu.Lock()
	if m.vfsGroup == nil {
		if m.state.Load() == dfsNew {
			m.move(dfsNew, dfsStopped)
		}
		m.mu.Unlock()
		m.logger.Info().Msg("Backend not initialized, nothing to stop")
		return nil
	}
	vfsG, backG := m.vfsGroup, m.backendGroup
	m.mu.Unlock()

	m.logger.Info().
		Str("backend", string(m.defaultBackendType)).
		Msg("Stopping FUSE filesystem")

	ctx, cancel := m.slice().Bound(vfsG.Context(), stopBudget)
	defer cancel()
	var errs []error
	if err := backG.Stop(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := vfsG.Stop(ctx); err != nil {
		errs = append(errs, err)
	}
	m.advance([]dfsState{dfsReady, dfsStarting, dfsFailed}, dfsStopping)
	if len(errs) > 0 {
		// A group outlived the budget: the unmount was never seen to finish, so the state stays Stopping.
		return errors.Join(errs...)
	}
	m.advance([]dfsState{dfsStopping}, dfsStopped)
	return nil
}

func (m *Manager) IsReady() bool {
	return m.state.Load() == dfsReady
}

var errNotReady = errors.New("DFS mount is not ready")

func (m *Manager) Refresh(dirs []string) error {
	if !m.IsReady() {
		return errNotReady
	}
	for _, dir := range dirs {
		m.backend.Refresh(dir)
	}
	return nil
}

// Stats returns unified statistics across all DFS mounts.
func (m *Manager) Stats() map[string]any {
	cur := m.state.Load()
	ready := cur == dfsReady
	stats := map[string]any{
		"enabled": true,
		"ready":   ready,
		"state":   cur.String(),
		"type":    m.Type(),
		"backend": string(m.defaultBackendType),
	}
	if cause := m.failure.Load(); cur == dfsFailed && cause != nil {
		stats["error"] = *cause
	}
	// Start sets vfs before the owner moves to Ready, so vfs is only read once ready.
	if ready && m.vfs != nil {
		maps.Copy(stats, m.vfs.GetStats())
	}
	return stats
}

func (m *Manager) CleanupCache() (map[string]any, error) {
	if !m.IsReady() {
		return nil, errNotReady
	}
	return m.vfs.CleanupCache(), nil
}

func (m *Manager) PurgeCache() (map[string]any, error) {
	if !m.IsReady() {
		return nil, errNotReady
	}
	return m.vfs.PurgeCache(), nil
}

func (m *Manager) Type() string {
	return "dfs"
}

var _ manager.RefusalReporter = (*Manager)(nil)

// Refusals reports how many illegal moves the manager's state refused.
func (m *Manager) Refusals() map[string]uint64 {
	return map[string]uint64{"dfs": m.state.Refused()}
}
