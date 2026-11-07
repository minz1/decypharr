package manager

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
)

// testStopTimeout bounds the stop of a test group.
const testStopTimeout = time.Minute

// testGroup returns a running task group, stopped when the test ends. A test
// that builds a Manager by hand gives it one as work. Like main's root it does
// not end with the test's context, so its children stop in order, not at once.
func testGroup(t testing.TB) *task.Group {
	t.Helper()
	g := task.NewRoot(context.WithoutCancel(t.Context()), "test", zerolog.Nop())
	t.Cleanup(func() {
		ctx, cancel := task.Detached(t.Context(), testStopTimeout)
		defer cancel()
		if err := g.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return g
}

// closePlacements closes the manager's placements group as Stop does: it waits for the provider deletions in
// flight and refuses every later one.
func closePlacements(t testing.TB, m *Manager) {
	t.Helper()
	ctx, cancel := task.Detached(t.Context(), testStopTimeout)
	defer cancel()
	if err := m.placementsSvc.group.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

// drainPlacements waits for the provider deletions started so far, then gives the manager a fresh placements
// group, so a test can start more.
func drainPlacements(t testing.TB, m *Manager) {
	t.Helper()
	closePlacements(t, m)
	m.placementsSvc = &service{group: testGroup(t)}
}

// waitWorkIdle waits until the manager's work group runs nothing but the job
// queue's own workers: every download action and background call has
// returned. It replaces waiting on the old download WaitGroup.
func waitWorkIdle(t testing.TB, m *Manager) {
	t.Helper()
	for range 10000 {
		busy := false
		for _, info := range m.work.Snapshot() {
			if !strings.HasSuffix(info.Path, ":worker") && !strings.HasSuffix(info.Path, ":closer") {
				busy = true
				break
			}
		}
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("work still running: %v", m.work.Snapshot())
}
