package manager

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/state/statetest"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
)

const lifecycleWait = 10 * time.Second

func TestLifecycleTable(t *testing.T) {
	t.Parallel()
	statetest.Table(t, lifecycleSuccessors,
		[]lifecycle{
			lifecycleConstructed, lifecycleStarting, lifecycleReady, lifecycleFailed, lifecycleStopping,
			lifecycleStopped,
		},
		[]statetest.Edge[lifecycle]{
			{From: lifecycleConstructed, To: lifecycleStarting},
			{From: lifecycleConstructed, To: lifecycleStopping},
			{From: lifecycleStarting, To: lifecycleReady},
			{From: lifecycleStarting, To: lifecycleFailed},
			{From: lifecycleStarting, To: lifecycleStopping},
			{From: lifecycleReady, To: lifecycleStopping},
			{From: lifecycleFailed, To: lifecycleStopping},
			{From: lifecycleStopping, To: lifecycleStopped},
		})
}

// newLifecycleManager builds a manager the way main does, on a root of its own.
func newLifecycleManager(t *testing.T) (*Manager, *task.Group) {
	t.Helper()
	root := task.NewRoot(t.Context(), "test", zerolog.Nop())
	m, err := New(root.Child("manager"), testConfigStore(t, nil), logger.Discard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := task.Detached(t.Context(), testStopTimeout)
		defer cancel()
		if stopErr := root.Stop(ctx); stopErr != nil {
			t.Error(stopErr)
		}
	})
	return m, root
}

func stopRoot(t *testing.T, root *task.Group) {
	t.Helper()
	ctx, cancel := task.Detached(t.Context(), testStopTimeout)
	defer cancel()
	if err := root.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestStartReachesReadyAndStopLeavesNoTasks(t *testing.T) {
	t.Parallel()
	m, root := newLifecycleManager(t)
	select {
	case <-m.IsReady():
		t.Fatal("ready before Start")
	default:
	}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.IsReady():
	case <-time.After(lifecycleWait):
		t.Fatalf("never ready; state %v", m.life.Load())
	}
	if got := m.life.Load(); got != lifecycleReady {
		t.Fatalf("state = %v, want ready", got)
	}
	if err := m.Start(); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start = %v, want ErrAlreadyStarted", err)
	}

	stopRoot(t, root)

	if left := root.Snapshot(); len(left) != 0 {
		t.Fatalf("tasks left after Stop: %v", left)
	}
	if got := m.life.Load(); got != lifecycleStopped {
		t.Fatalf("state after Stop = %v, want stopped", got)
	}
	if err := m.Start(); !errors.Is(err, task.ErrStopped) {
		t.Fatalf("Start after Stop = %v, want ErrStopped", err)
	}
	if err := m.runWork("late", func() { t.Error("work ran after Stop") }); !errors.Is(err, task.ErrStopped) {
		t.Fatalf("work after Stop = %v, want ErrStopped", err)
	}
	if err := m.SubmitJob(&Job{ID: "late"}); !errors.Is(err, task.ErrStopped) {
		t.Fatalf("SubmitJob after Stop = %v, want ErrStopped", err)
	}
	if m.Context().Err() == nil {
		t.Fatal("the manager's context is live after Stop")
	}
}

func TestStopWithoutStartReleasesWhatNewOpened(t *testing.T) {
	t.Parallel()
	m, root := newLifecycleManager(t)

	stopRoot(t, root)

	if left := root.Snapshot(); len(left) != 0 {
		t.Fatalf("tasks left after Stop: %v", left)
	}
	if got := m.life.Load(); got != lifecycleStopped {
		t.Fatalf("state = %v, want stopped", got)
	}
	select {
	case <-m.IsReady():
		t.Fatal("a manager that never started became ready")
	default:
	}
}

// A start step that fails fails the manager's group: the services that had started stop, ready never fires, and the
// failure reaches the root so its owner sees it.
func TestPartialStartFailureFailsTheGroup(t *testing.T) {
	t.Parallel()
	m, root := newLifecycleManager(t)
	if m.arrService == nil || m.arrIndexer == nil {
		t.Fatal("the Arr services are not configured in the test manager")
	}
	// The reacquisition service starts; the indexer, closed beforehand, refuses.
	m.arrIndexer.Close()

	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-root.Failed():
	case <-time.After(lifecycleWait):
		t.Fatalf("the root did not fail; state %v", m.life.Load())
	}
	if err := root.Err(); err == nil || !strings.Contains(err.Error(), "Arr indexer") {
		t.Fatalf("root error = %v, want the indexer's start failure", err)
	}
	stopRoot(t, root)

	select {
	case <-m.IsReady():
		t.Fatal("ready fired after a failed start")
	default:
	}
	if got := m.life.Load(); got != lifecycleStopped {
		t.Fatalf("state after the stop = %v, want stopped", got)
	}
	if err := m.arrService.Start(m.work.Context()); !errors.Is(err, reacquire.ErrServiceClosed) {
		t.Fatalf("the started reacquisition service was not stopped: Start = %v", err)
	}
	if left := root.Snapshot(); len(left) != 0 {
		t.Fatalf("tasks left after the stop: %v", left)
	}
}

// Services stop in reverse registration order, so what the others rely on outlives them.
func TestServicesStopNewestFirst(t *testing.T) {
	t.Parallel()
	m, root := newLifecycleManager(t)
	var order []string
	record := func(name string) func() error {
		return func() error {
			order = append(order, name)
			return nil
		}
	}
	for _, name := range []string{"first", "second", "third"} {
		if _, err := m.superviseService(name, nil, record(name)); err != nil {
			t.Fatal(err)
		}
	}

	stopRoot(t, root)

	if want := []string{"third", "second", "first"}; !slices.Equal(order, want) {
		t.Fatalf("stop order = %v, want %v", order, want)
	}
}
