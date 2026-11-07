package manager

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/storage/migration"
)

// A stop that arrives while the start sequence waits on a service is not a failure: the start task ends cleanly, so
// the stop leaves the root clean (Err is nil) instead of logging "Task failed during stop" and marking it dirty.
func TestStopDuringStartIsNotAFailure(t *testing.T) {
	t.Parallel()
	m := newShutdownTestManager(t, t.TempDir())
	m.migrator = migration.New(m.storage, t.TempDir(), zerolog.Nop())
	waiting := make(chan struct{})
	slow, err := m.superviseService("slow", func(ctx context.Context) error {
		close(waiting)
		<-ctx.Done()
		return context.Cause(ctx)
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.reacquireSvc = slow
	if err = m.Start(); err != nil {
		t.Fatal(err)
	}
	<-waiting

	stopRoot(t, m.group)

	if err = m.group.Err(); err != nil {
		t.Fatalf("a stop during Start left the group failed: %v", err)
	}
	select {
	case <-m.IsReady():
		t.Fatal("ready fired for a start that was stopped")
	default:
	}
	select {
	case <-m.group.Failed():
		t.Fatal("the group failed")
	default:
	}
}
