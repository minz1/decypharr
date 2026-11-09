package manager

import (
	"context"
	"testing"
	"testing/synctest"
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
// group, so a test can start more. It runs in a synctest bubble: [synctest.Wait] lets every started run finish
// before the group's stop cancels the runs' ctx, which a run not yet at its check would read as a stop.
func drainPlacements(t testing.TB, m *Manager) {
	t.Helper()
	synctest.Wait()
	closePlacements(t, m)
	m.placementsSvc = &service{group: testGroup(t)}
}
