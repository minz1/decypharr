package keyed_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

// A child task that panics is recovered, logged and counted, and the message onPanic builds goes back to the key
// even though the mailbox is full: an actor waiting for the child is never left waiting.
func TestAChildPanicDeliversTheOnPanicMessage(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		logs := &syncBuffer{}
		root := task.NewRoot(context.WithoutCancel(t.Context()), "test", zerolog.Nop())
		t.Cleanup(func() { stop(t, root) })
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](root, "child-panic", rec, keyed.Options[msg]{
			MailboxSize: 1,
			Logger:      zerolog.New(logs),
		})
		release := make(chan struct{})
		fail := make(chan struct{})
		mustSend(t, r, "a", msg{id: 1, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			err := fx.Go("io", func(context.Context) msg {
				<-fail
				panic("provider exploded")
			}, func(v any) msg {
				if v != "provider exploded" {
					t.Errorf("onPanic got %v", v)
				}
				return msg{id: 99}
			})
			if err != nil {
				t.Error(err)
			}
			<-release
		}})
		synctest.Wait()
		mustSend(t, r, "a", msg{id: 2}) // the mailbox is now full
		close(fail)
		synctest.Wait()

		close(release)
		synctest.Wait()

		if got := rec.got("a"); !slices.Equal(got, []int{1, 2, 99}) {
			t.Fatalf("handled %v, want the onPanic message queued past the bound: [1 2 99]", got)
		}
		if got := r.Snapshot().ChildPanics; got != 1 {
			t.Fatalf("child panics = %d, want 1", got)
		}
		if !strings.Contains(logs.String(), "provider exploded") || !strings.Contains(logs.String(), `"key":"a"`) {
			t.Fatalf("the child's panic was not logged with its key: %s", logs.String())
		}
		select {
		case <-root.Failed():
			t.Fatal("a child's panic failed the group")
		default:
		}
	})
}

func TestAChildPanicWithNoOnPanicDeliversNothing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](newRoot(t), "child-panic-none", rec, keyed.Options[msg]{})
		mustSend(t, r, "a", msg{id: 1, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			_ = fx.Go("io", func(context.Context) msg { panic("boom") }, nil)
		}})
		synctest.Wait()

		if got := rec.got("a"); !slices.Equal(got, []int{1}) {
			t.Fatalf("handled %v, want only the first message", got)
		}
		if got := r.Snapshot().ChildPanics; got != 1 {
			t.Fatalf("child panics = %d, want 1", got)
		}
		if keys := r.Snapshot().Keys; len(keys) != 1 || keys[0].Children != 0 {
			t.Fatalf("keys = %+v, want the child no longer in flight", keys)
		}
	})
}

// onPanic is code too: a panic in it is contained, and the key goes on.
func TestAPanicInOnPanicIsContained(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](newRoot(t), "on-panic-panics", rec, keyed.Options[msg]{})
		mustSend(t, r, "a", msg{id: 1, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			_ = fx.Go("io", func(context.Context) msg { panic("boom") }, func(any) msg { panic("again") })
		}})
		synctest.Wait()
		mustSend(t, r, "a", msg{id: 2})
		synctest.Wait()

		if got := rec.got("a"); !slices.Equal(got, []int{1, 2}) {
			t.Fatalf("handled %v, want the key still serving: [1 2]", got)
		}
	})
}
