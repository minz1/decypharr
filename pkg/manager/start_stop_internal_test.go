package manager

import (
	"testing"
	"testing/synctest"

	"github.com/rs/zerolog"
)

// A stop that arrives while the start task is still running (it is blocked in a startup step that has not seen the stop yet)
// must not close storage under it: storage closes only once the start task has returned.
func TestStorageClosesOnlyAfterTheStartTaskReturns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := newShutdownTestManager(t, t.TempDir())
		logs := &lockedLog{}
		m.logger = zerolog.New(logs)
		// Start has been called and its task has not returned.
		m.startRan = true

		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			stopRoot(t, m.group)
		}()
		synctest.Wait()
		if logs.contains("Closing storage database") {
			t.Fatal("storage closed while the start task was still running")
		}

		close(m.startDone)
		<-stopped
		if !logs.contains("Closing storage database") {
			t.Fatal("storage was not closed after the start task returned")
		}
	})
}

// A stop that begins before Start is called refuses Start, so storage does not wait for a task that never runs.
func TestStopBeforeStartDoesNotWaitForTheStartTask(t *testing.T) {
	t.Parallel()
	m := newShutdownTestManager(t, t.TempDir())

	stopRoot(t, m.group)

	if err := m.Start(); err == nil {
		t.Fatal("Start succeeded after the stop")
	}
	if got := m.life.Load(); got != lifecycleStopped {
		t.Fatalf("state = %v, want stopped", got)
	}
}
