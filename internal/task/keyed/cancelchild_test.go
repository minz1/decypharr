package keyed_test

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

// CancelChild cancels the child's context and drops what it returns: a provider call the actor gave up on
// cannot report back into a state that has moved on.
func TestCancelChildEndsTheChildsContextAndDropsItsResult(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](newRoot(t), "cancel-child", rec, keyed.Options[msg]{})
		ended := make(chan error, 1)
		ids := make(chan keyed.ChildID, 1)
		mustSend(t, r, "a", msg{id: 1, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			id, err := fx.Go("io", func(ctx context.Context) msg {
				<-ctx.Done()
				ended <- ctx.Err()
				return msg{id: 2} // returned after the cancel: dropped
			}, nil)
			if err != nil {
				t.Error(err)
			}
			ids <- id
		}})
		synctest.Wait()

		mustSend(t, r, "a", msg{id: 3, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			fx.CancelChild(<-ids)
		}})
		synctest.Wait()

		if err := <-ended; err == nil {
			t.Fatal("the child's context did not end")
		}
		if got := rec.got("a"); !slices.Equal(got, []int{1, 3}) {
			t.Fatalf("handled %v, want [1 3]: the cancelled child's result is dropped", got)
		}
		if keys := r.Snapshot().Keys; len(keys) != 1 || keys[0].Children != 0 {
			t.Fatalf("keys = %+v, want no child in flight", keys)
		}
	})
}

// A child that ignores its context must not hold the key up once the actor has cancelled it.
func TestACancelledStuckChildDoesNotKeepTheKeyUp(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		g := newRoot(t)
		r := keyed.New[string, msg, struct{}](g, "stuck", rec, keyed.Options[msg]{IdleAfter: time.Second})
		release := make(chan struct{})
		mustSend(t, r, "a", msg{id: 1, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			id, _ := fx.Go("io", func(context.Context) msg {
				<-release // ignores the context
				return msg{id: 2}
			}, nil)
			fx.CancelChild(id)
		}})
		synctest.Wait()

		time.Sleep(5 * time.Second)
		synctest.Wait()

		if keys := r.Snapshot().Keys; len(keys) != 0 {
			t.Fatalf("keys = %+v, want the key gone although the stuck child still runs", keys)
		}
		close(release)
		synctest.Wait()
		if got := rec.got("a"); !slices.Equal(got, []int{1}) {
			t.Fatalf("handled %v, want [1]: the stuck child's late result is dropped", got)
		}
	})
}

// A child that finished while the actor was busy has its result queued; cancelling the child then takes the
// result back out, so the actor never acts on work it gave up on.
func TestCancelChildRecallsAResultAlreadyQueued(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](newRoot(t), "cancel-queued-child", rec, keyed.Options[msg]{})
		proceed := make(chan struct{})
		var id keyed.ChildID
		mustSend(t, r, "a", msg{id: 1, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			id, _ = fx.Go("io", func(context.Context) msg {
				<-proceed
				return msg{id: 2}
			}, nil)
		}})
		synctest.Wait()

		release := make(chan struct{})
		mustSend(t, r, "a", msg{id: 3, block: release, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			fx.CancelChild(id)
		}})
		synctest.Wait() // msg 3 is in flight
		close(proceed)
		synctest.Wait() // the child's result waits behind msg 3
		if depth := r.Snapshot().Keys[0].Depth; depth != 1 {
			t.Fatalf("depth = %d, want the child's result queued", depth)
		}

		close(release)
		synctest.Wait()

		if got := rec.got("a"); !slices.Equal(got, []int{1, 3}) {
			t.Fatalf("handled %v, want [1 3]: a cancelled child's queued result is dropped", got)
		}
		if keys := r.Snapshot().Keys; len(keys) != 1 || keys[0].Depth != 0 || keys[0].Children != 0 {
			t.Fatalf("keys = %+v, want nothing queued and no child in flight", keys)
		}
	})
}

func TestCancelChildOfAnUnknownOrFinishedChildChangesNothing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](newRoot(t), "cancel-unknown", rec, keyed.Options[msg]{})
		ids := make(chan keyed.ChildID, 1)
		mustSend(t, r, "a", msg{id: 1, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			id, _ := fx.Go("io", func(context.Context) msg { return msg{id: 2} }, nil)
			ids <- id
		}})
		synctest.Wait()

		mustSend(t, r, "a", msg{id: 3, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			fx.CancelChild(<-ids) // it has finished and delivered
			fx.CancelChild(keyed.ChildID(999))
		}})
		synctest.Wait()

		if got := rec.got("a"); !slices.Equal(got, []int{1, 2, 3}) {
			t.Fatalf("handled %v, want [1 2 3]", got)
		}
	})
}
