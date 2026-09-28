package nntp

import (
	"context"
	"errors"
	"testing"
)

// A chunk accepted by Submit must always get its done callback, even when
// Stop wins the race against the workers; BatchStat waits on it.
func TestRepairPoolStopCompletesQueuedTasks(t *testing.T) {
	t.Parallel()
	// No workers, so the task is still queued when Stop runs.
	p := &RepairPool{tasks: make(chan repairTask, 1), quit: make(chan struct{})}
	got := make(chan error, 1)
	if err := p.Submit(context.Background(), []string{"a@b"}, func(_ []StatResult, err error) {
		got <- err
	}); err != nil {
		t.Fatal(err)
	}
	p.Stop()
	select {
	case err := <-got:
		if !errors.Is(err, errRepairPoolClosed) {
			t.Fatalf("done err = %v, want errRepairPoolClosed", err)
		}
	default:
		t.Fatal("queued task abandoned by Stop: its done callback never ran")
	}
	err := p.Submit(context.Background(), nil, func([]StatResult, error) {})
	if !errors.Is(err, errRepairPoolClosed) {
		t.Fatalf("Submit after Stop = %v, want errRepairPoolClosed", err)
	}
}
