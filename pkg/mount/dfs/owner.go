package dfs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/backend"
)

const (
	// stopBudget bounds the whole DFS stop when no mount slice is armed (a stop
	// outside the generation): the unmount, then the cache close.
	stopBudget = 10 * time.Second
	// gracefulUnmountTimeout is how long the graceful unmount may take before
	// the mount point is lazily detached.
	gracefulUnmountTimeout = 4 * time.Second
	// vfsCloseReserve is the part of stopBudget kept for closing the cache: the
	// detach context ends this long before the budget does.
	vfsCloseReserve = 2 * time.Second
	// detachFloor is the least time a lazy detach gets, even past the budget:
	// a mount left attached outlives the process.
	detachFloor = time.Second
)

var (
	// errStarted is Start's refusal when the manager was started already.
	errStarted = errors.New("DFS already started")
	// errMountDied fails the generation when the serve loop ends under a
	// mounted filesystem that nobody asked to stop.
	errMountDied = errors.New("DFS mount died: serve loop ended while mounted")
)

// startResult carries the owner's verdict on Start: closed once, with the
// failure or nil for Ready.
type startResult struct {
	once sync.Once
	done chan struct{}
	err  error
}

func newStartResult() *startResult { return &startResult{done: make(chan struct{})} }

func (r *startResult) report(err error) {
	r.once.Do(func() {
		r.err = err
		close(r.done)
	})
}

// mountReady mounts and moves Starting to Ready. It returns nil, with the
// start result reported, when the mount failed, was cancelled, or came up late
// (and was released).
func (m *Manager) mountReady(
	ctx context.Context,
	bck backend.Backend,
	backG *task.Group,
	res *startResult,
) backend.Mounted {
	h, err := bck.Mount(ctx, backG)
	switch {
	case err != nil && ctx.Err() != nil:
		// A stop cancelled the mount: that is a stop, not a failure.
		m.advance([]dfsState{dfsStarting}, dfsStopping)
		res.report(task.ErrStopped)
		return nil
	case err != nil:
		// A start failure is not a task failure: the manager stays up and
		// shows state failed.
		m.advance([]dfsState{dfsStarting}, dfsFailed)
		res.report(fmt.Errorf("failed to start DFS backend: %w", err))
		return nil
	}
	select {
	case <-h.Done():
		m.advance([]dfsState{dfsStarting}, dfsFailed)
		res.report(errors.New("DFS serve loop ended before the mount was ready"))
		return nil
	default:
	}
	if ctx.Err() != nil || !m.move(dfsStarting, dfsReady) {
		// A stop, or Start's timeout, got there first: the mount came up
		// late and nobody will use it.
		m.advance([]dfsState{dfsStarting}, dfsStopping)
		m.unmountLate(ctx, h)
		res.report(task.ErrStopped)
		return nil
	}
	return h
}

// owner is the backend group's task: it mounts, watches the serve loop and
// unmounts, and it is the only task that touches the mount.
func (m *Manager) owner(bck backend.Backend, backG *task.Group, res *startResult) func(context.Context) error {
	return func(ctx context.Context) error {
		defer res.report(task.ErrStopped)

		h := m.mountReady(ctx, bck, backG, res)
		if h == nil {
			return nil
		}

		begin := make(chan struct{})
		abandon := make(chan struct{})
		unmounted := make(chan error, 1)
		defer close(abandon)
		unmountErr := backG.Go("unmount", func(context.Context) error {
			select {
			case <-begin:
				unmounted <- h.Unmount()
			case <-abandon:
			}
			return nil
		})
		if unmountErr != nil {
			// The group is stopping: unmount without the graceful step.
			m.stopMount(ctx, h, nil, nil)
			res.report(task.ErrStopped)
			return nil
		}
		res.report(nil)

		select {
		case <-h.Done():
			// The ctx check is defence in depth against an outside serve end
			// racing a stop: our own unmount cannot reach it, because begin
			// closes only after this select.
			if ctx.Err() == nil && m.move(dfsReady, dfsFailed) {
				m.logger.Error().
					Str("mount_path", m.config.MountPath).
					Str("backend", string(m.defaultBackendType)).
					Msg("DFS mount died: serve loop ended while mounted")
				return errMountDied
			}
		case <-ctx.Done():
		}
		m.stopMount(ctx, h, begin, unmounted)
		return nil
	}
}

// stopMount is the owner's stop steps: the state goes to Stopping, then the
// graceful unmount runs for up to gracefulUnmountTimeout, then a lazy detach
// for what is left of the budget minus the cache's reserve. begin is nil when
// no unmount task could be started.
func (m *Manager) stopMount(ctx context.Context, h backend.Mounted, begin chan struct{}, unmounted <-chan error) {
	sctx, cancel := m.slice().Bound(ctx, stopBudget)
	defer cancel()

	m.advance([]dfsState{dfsReady, dfsStarting}, dfsStopping)
	// Only now may an unmount begin: a serve end caused by it meets Stopping.
	reason := "unmount task unavailable"
	if begin != nil {
		close(begin)
		timer := time.NewTimer(gracefulUnmountTimeout)
		defer timer.Stop()
		select {
		case err := <-unmounted:
			if err == nil {
				m.logUnmounted("graceful", "")
				return
			}
			reason = err.Error()
		case <-timer.C:
			reason = "timeout"
		case <-sctx.Done():
			reason = "timeout"
		}
	}
	m.detach(sctx, h, reason)
}

// unmountLate releases a mount that came up after the owner was told to stop:
// there is no unmount task (the group is stopping), so the owner unmounts it.
func (m *Manager) unmountLate(ctx context.Context, h backend.Mounted) {
	sctx, cancel := m.slice().Bound(ctx, stopBudget)
	defer cancel()
	if err := h.Unmount(); err != nil {
		m.detach(sctx, h, err.Error())
		return
	}
	m.logUnmounted("graceful", "")
}

// detach lazily detaches the mount, bounded to leave the cache its reserve
// but never given less than detachFloor: like the rcd reap, it keeps its own
// time when the slice is (nearly) over, since an attached mount outlives us.
func (m *Manager) detach(sctx context.Context, h backend.Mounted, reason string) {
	dctx, cancel := sctx, context.CancelFunc(func() {})
	if deadline, ok := sctx.Deadline(); ok {
		end := deadline.Add(-vfsCloseReserve)
		if floor := time.Now().Add(detachFloor); end.Before(floor) {
			dctx, cancel = context.WithDeadline(context.WithoutCancel(sctx), floor)
		} else {
			dctx, cancel = context.WithDeadline(sctx, end)
		}
	}
	defer cancel()
	if err := h.Detach(dctx); err != nil {
		m.logger.Warn().Err(err).Msg("DFS detach failed")
	}
	m.logUnmounted("forced", reason)
}

func (m *Manager) logUnmounted(branch, reason string) {
	ev := m.logger.Info().
		Str("mount_path", m.config.MountPath).
		Str("backend", string(m.defaultBackendType)).
		Str("branch", branch)
	if reason != "" {
		ev = ev.Str("reason", reason)
	}
	ev.Msg("DFS unmounted")
}

// closer is the vfs group's task: once the backend group is done (the owner
// has unmounted, or never mounted) it closes the cache.
func (m *Manager) closer(vfsG, backG *task.Group) func(context.Context) error {
	return func(ctx context.Context) error {
		select {
		case <-backG.Done():
		case <-ctx.Done():
		}
		m.advance([]dfsState{dfsReady, dfsStarting}, dfsStopping)
		m.releaseVFS()
		m.advance([]dfsState{dfsStopping}, dfsStopped)
		// Ends the evict and speed tasks too, on the start-failure path.
		vfsG.Close()
		return nil
	}
}

// releaseVFS closes the cache.
func (m *Manager) releaseVFS() {
	if err := m.closeVFS(); err != nil {
		m.logger.Warn().Err(err).Msg("Failed to close VFS")
	}
}
