package keyed_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

// exitMsg turns a tracked child's exit error into a message id the recorder can show.
func exitMsg(err error) msg {
	var p *keyed.ChildPanicError
	switch {
	case errors.Is(err, keyed.ErrChildCancelled):
		return msg{id: 9}
	case errors.As(err, &p):
		return msg{id: 10}
	default:
		return msg{id: 11}
	}
}

// A tracked child the actor cancels still reports what it returned: a provider POST that lands after a delete
// must reach the actor so its placement can be released.
func TestTrackedChildResultArrivesAfterCancelAwait(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](newRoot(t), "tracked-late", rec, keyed.Options[msg]{})
		ids := make(chan keyed.ChildID, 1)
		ended := make(chan error, 1)
		mustSend(t, r, "a", msg{id: 1, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			id, err := fx.GoTracked("io", func(ctx context.Context) msg {
				<-ctx.Done()
				ended <- ctx.Err()
				return msg{id: 2}
			}, exitMsg)
			if err != nil {
				t.Error(err)
			}
			ids <- id
		}})
		synctest.Wait()

		mustSend(t, r, "a", msg{id: 3, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			fx.CancelAwait(<-ids)
		}})
		synctest.Wait()

		if err := <-ended; err == nil {
			t.Fatal("the child's context did not end")
		}
		if got := rec.got("a"); !slices.Equal(got, []int{1, 3, 2}) {
			t.Fatalf("handled %v, want [1 3 2]: the cancelled child's result is delivered", got)
		}
	})
}

// Cancelled in the same handler that started it, a tracked child reports exactly one exit: either fn's result
// (it had started) or ErrChildCancelled (it had not).
func TestTrackedChildCancelledAtOnceReportsExactlyOneExit(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](newRoot(t), "tracked-early", rec, keyed.Options[msg]{})
		mustSend(t, r, "a", msg{id: 1, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			id, err := fx.GoTracked("io", func(ctx context.Context) msg {
				<-ctx.Done()
				return msg{id: 2}
			}, exitMsg)
			if err != nil {
				t.Error(err)
			}
			fx.CancelAwait(id)
		}})
		synctest.Wait()

		got := rec.got("a")
		if len(got) != 2 || got[0] != 1 || (got[1] != 2 && got[1] != 9) {
			t.Fatalf("handled %v, want [1 2] or [1 9]", got)
		}
	})
}

func TestTrackedChildPanicReportsChildPanicError(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](newRoot(t), "tracked-panic", rec, keyed.Options[msg]{})
		mustSend(t, r, "a", msg{id: 1, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			if _, err := fx.GoTracked("io", func(context.Context) msg { panic("boom") }, exitMsg); err != nil {
				t.Error(err)
			}
		}})
		synctest.Wait()

		if got := rec.got("a"); !slices.Equal(got, []int{1, 10}) {
			t.Fatalf("handled %v, want [1 10]: a panicked tracked child reports a ChildPanicError", got)
		}
	})
}

// A cancelled tracked child that ignores its context keeps the key up until its exit is handled, so the actor's
// state is still there when the late result arrives.
func TestTrackedChildKeepsTheKeyAliveUntilItsExitIsHandled(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](
			newRoot(t),
			"tracked-alive",
			rec,
			keyed.Options[msg]{IdleAfter: time.Second},
		)
		release := make(chan struct{})
		ids := make(chan keyed.ChildID, 1)
		mustSend(t, r, "a", msg{id: 1, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			id, _ := fx.GoTracked("io", func(context.Context) msg {
				<-release // ignores the context
				return msg{id: 2}
			}, exitMsg)
			ids <- id
		}})
		synctest.Wait() // the child has started and blocks
		mustSend(t, r, "a", msg{id: 3, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			fx.CancelAwait(<-ids)
		}})
		synctest.Wait()

		time.Sleep(5 * time.Second)
		synctest.Wait()
		if keys := r.Snapshot().Keys; len(keys) != 1 || keys[0].Children != 1 {
			t.Fatalf("keys = %+v, want the key up with its child counted", keys)
		}

		close(release)
		synctest.Wait()
		if got := rec.got("a"); !slices.Equal(got, []int{1, 3, 2}) {
			t.Fatalf("handled %v, want [1 3 2]", got)
		}
	})
}
