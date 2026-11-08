package task_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/task"
)

func TestGraceOutlivesParentByD(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		parent, stop := context.WithCancel(t.Context())
		ctx, cancel := task.Grace(parent, 5*time.Second)
		defer cancel()

		time.Sleep(10 * time.Second)
		stop()
		synctest.Wait()
		if ctx.Err() != nil {
			t.Fatalf("grace ctx ended with its parent: %v", ctx.Err())
		}

		time.Sleep(5*time.Second - time.Nanosecond)
		synctest.Wait()
		if ctx.Err() != nil {
			t.Fatalf("grace ctx ended before parent end + d: %v", ctx.Err())
		}

		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("grace ctx err = %v at parent end + d, want Canceled", ctx.Err())
		}
		if !errors.Is(context.Cause(ctx), task.ErrGraceExpired) {
			t.Fatalf("grace ctx cause = %v, want ErrGraceExpired", context.Cause(ctx))
		}
	})
}

func TestGraceNoDeadlineWhileParentLive(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := task.Grace(t.Context(), 5*time.Second)
		defer cancel()

		if _, ok := ctx.Deadline(); ok {
			t.Fatal("grace ctx has a deadline while its parent is live")
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if ctx.Err() != nil {
			t.Fatalf("grace ctx ended while its parent is live: %v", ctx.Err())
		}
	})
}

func TestGraceCancelFuncReleases(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		parent, stop := context.WithCancel(t.Context())
		ctx, cancel := task.Grace(parent, 5*time.Second)

		cancel()
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("grace ctx err = %v after its CancelFunc, want Canceled", ctx.Err())
		}
		if errors.Is(context.Cause(ctx), task.ErrGraceExpired) {
			t.Fatal("a CancelFunc cancel reads as an expired grace")
		}
		// The parent ending later must arm nothing: no grace timer fires.
		stop()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if errors.Is(context.Cause(ctx), task.ErrGraceExpired) {
			t.Fatal("a grace timer fired after the CancelFunc")
		}
	})
}

func TestGraceCancelFuncAfterParentEnd(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		parent, stop := context.WithCancel(t.Context())
		ctx, cancel := task.Grace(parent, 5*time.Second)

		stop()
		synctest.Wait()
		cancel()
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("grace ctx err = %v after its CancelFunc, want Canceled", ctx.Err())
		}
	})
}
