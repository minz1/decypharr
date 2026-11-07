package manager

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/task"
)

// slowStartMount blocks in Start until its context ends, as a mount does while it waits for the mountpoint.
type slowStartMount struct {
	MountManager

	started chan struct{}
	record  func(string)
}

func (m slowStartMount) Start(g *task.Group) error {
	ctx := g.Context()
	close(m.started)
	<-ctx.Done()
	m.record("start returned")
	return context.Cause(ctx)
}

func (m slowStartMount) Stop() error {
	m.record("stop")
	return nil
}

// A mount whose Start is still running when the manager stops is told to stop only after its Start returned, once,
// and the stop does not wait for anything but that.
func TestMountStoppedMidStartIsStoppedAfterStartReturns(t *testing.T) {
	t.Parallel()
	m := newShutdownTestManager(t, t.TempDir())
	var mu sync.Mutex
	var events []string
	m.mountManager = slowStartMount{started: make(chan struct{}), record: func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}}
	starting := make(chan struct{})
	if err := m.group.Go("start-mount", func(ctx context.Context) error {
		close(starting)
		m.startMount(ctx)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-starting
	<-m.mountManager.(slowStartMount).started

	ctx, cancel := task.Detached(t.Context(), 10*time.Second)
	defer cancel()
	if err := m.group.Stop(ctx); err != nil {
		t.Fatalf("Stop = %v, want it to finish within the budget", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"start returned", "stop"}; !slices.Equal(events, want) {
		t.Fatalf("mount events = %v, want %v", events, want)
	}
}
