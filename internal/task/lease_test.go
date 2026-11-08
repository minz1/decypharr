package task_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
)

func TestEnterRefusedWhenStopped(t *testing.T) {
	t.Parallel()
	group, _ := newRoot(t)
	stop(t, group)
	leave, err := group.Enter("fetch")
	if !errors.Is(err, task.ErrStopped) || leave != nil {
		t.Fatalf("err=%v leave=%v, want ErrStopped and no leave", err, leave != nil)
	}
	child := group.Child("late")
	if _, err = child.Enter("fetch"); !errors.Is(err, task.ErrStopped) {
		t.Fatalf("born-stopped child: err=%v, want ErrStopped", err)
	}
}

func TestEnterRefusedAfterCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	group := task.NewRoot(ctx, "root", zerolog.Nop())
	cancel()
	if _, err := group.Enter("fetch"); !errors.Is(err, task.ErrStopped) {
		t.Fatalf("err=%v, want ErrStopped once the context ended", err)
	}
	stop(t, group)
}

func TestStopWaitsForLease(t *testing.T) {
	t.Parallel()
	group, _ := newRoot(t)
	leave, err := group.Enter("fetch")
	if err != nil {
		t.Fatalf("enter: %v", err)
	}
	var left atomic.Bool
	go func() {
		<-group.Context().Done()
		left.Store(true)
		leave()
	}()
	stop(t, group)
	if !left.Load() {
		t.Fatal("Stop returned before the lease was left")
	}
	if snap := group.Snapshot(); len(snap) != 0 {
		t.Fatalf("snapshot after stop = %v, want empty", snap)
	}
}

func TestLeaveIdempotent(t *testing.T) {
	t.Parallel()
	group, _ := newRoot(t)
	leave, err := group.Enter("fetch")
	if err != nil {
		t.Fatalf("enter: %v", err)
	}
	leave()
	leave()
	if snap := group.Snapshot(); len(snap) != 0 {
		t.Fatalf("snapshot = %v, want empty", snap)
	}
	stop(t, group)
}

func TestSnapshotListsLease(t *testing.T) {
	t.Parallel()
	group, _ := newRoot(t)
	leave, err := group.Child("test").Enter("fetch")
	if err != nil {
		t.Fatalf("enter: %v", err)
	}
	var paths []string
	for _, info := range group.Snapshot() {
		paths = append(paths, info.Path)
	}
	if want := []string{"root/test:fetch"}; !slices.Equal(paths, want) {
		t.Fatalf("snapshot = %v, want %v", paths, want)
	}
	leave()
	stop(t, group)
}
