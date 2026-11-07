package manager

import (
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
)

// testStopTimeout bounds the stop of a test group.
const testStopTimeout = time.Minute

// testGroup returns a running task group on t's context, stopped when the
// test ends. A test that builds a Manager by hand gives it one as work.
func testGroup(t testing.TB) *task.Group {
	t.Helper()
	g := task.NewRoot(t.Context(), "test", zerolog.Nop())
	t.Cleanup(func() {
		ctx, cancel := task.Detached(t.Context(), testStopTimeout)
		defer cancel()
		if err := g.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return g
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
