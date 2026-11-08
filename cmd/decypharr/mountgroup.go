package decypharr

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/manager"
)

// mountSliceBudget is the share of the generation's teardown the mount gets:
// the unmount, the signal to rcd, and the cache close fit in it when it is
// armed in full; a slice armed with under ~3s left lets the DFS detach run up
// to dfs's detachFloor (1s) past it, and the cache close after that.
const mountSliceBudget = 8 * time.Second

// mountSlice is the mount's time budget for its stop. It is unarmed until the
// stop reaches the mount, and then ends mountSliceBudget later or with the
// generation's stop context, whichever is first.
type mountSlice struct {
	gen *generation

	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	released bool
}

func newMountSlice(gen *generation) *mountSlice { return &mountSlice{gen: gen} }

// begin arms the slice, once. It is the only place that asks the generation
// for its stop context, which starts the drain timer.
func (s *mountSlice) begin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx != nil || s.released {
		return
	}
	s.ctx, s.cancel = context.WithTimeout(s.gen.stopContext(), mountSliceBudget)
}

// get is the task.Slice the mount owners take: (nil, false) before begin. It
// never touches the generation's stop context.
func (s *mountSlice) get() (context.Context, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctx, s.ctx != nil
}

// release frees the slice's timer once the teardown is over.
func (s *mountSlice) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = true
	if s.cancel != nil {
		s.cancel()
	}
}

// stopSliceTaker is a mount manager that bounds its stop waits by the slice.
type stopSliceTaker interface {
	SetStopSlice(task.Slice)
}

// backendStopTaker is a mount manager that wants to know when the WebDAV server it reads from stops.
type backendStopTaker interface {
	SetBackendStopped(<-chan struct{})
}

// startMountGroup makes the generation's mount group: "run" starts the mount
// once the manager is ready and stops it after its own children (the rcd, the
// DFS groups) have ended; "stopping", made last, so it stops first, arms the
// slice and closes the gate before anything of the mount stops. A failed mount
// start is logged and the generation goes on without a mount. backendStopped,
// closed when the WebDAV server stops, goes to a manager that takes it.
func startMountGroup(
	root *task.Group,
	mm manager.MountManager,
	ready <-chan struct{},
	gate *manager.MountGate,
	slice *mountSlice,
	backendStopped <-chan struct{},
	log zerolog.Logger,
) error {
	if mm == nil {
		return nil
	}
	if taker, ok := mm.(stopSliceTaker); ok {
		taker.SetStopSlice(slice.get)
	}
	if taker, ok := mm.(backendStopTaker); ok {
		taker.SetBackendStopped(backendStopped)
	}
	mountG := root.Child("mount")
	run := mountG.Child("run")
	stopping := mountG.Child("stopping")
	if err := run.Go("mount", func(ctx context.Context) error {
		select {
		case <-ready:
			if err := startMount(mm, run, log); err != nil {
				log.Error().Err(err).Msg("Failed to start mount manager, continuing without mounting")
			}
		case <-ctx.Done():
		}
		<-ctx.Done()
		log.Info().Msg("Stopping mount manager")
		if err := mm.Stop(); err != nil {
			log.Warn().Err(err).Msg("Failed to stop mount manager")
		}
		return nil
	}); err != nil {
		return fmt.Errorf("start the mount group: %w", err)
	}
	if err := stopping.Go("gate", func(ctx context.Context) error {
		<-ctx.Done()
		slice.begin()
		gate.BeginStop()
		return nil
	}); err != nil {
		return fmt.Errorf("start the mount stop gate: %w", err)
	}
	return nil
}

// startMount runs the mount's Start. A panic is logged with its stack and
// returned as the start's error, so the stop still runs.
func startMount(mm manager.MountManager, run *task.Group, log zerolog.Logger) error {
	var failure error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error().Interface("panic", recovered).Bytes("stack", debug.Stack()).Msg("Mount start panicked")
				failure = fmt.Errorf("mount start panicked: %v", recovered)
			}
		}()
		failure = mm.Start(run)
	}()
	return failure
}
