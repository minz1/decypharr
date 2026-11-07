package keyed_test

import (
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

// A deadline that fired but whose message still waits behind a busy handler is cancelled like any other: the
// actor must not act on a deadline it has already cancelled.
func TestCancellingATimerRemovesItsQueuedMessage(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](newRoot(t), "cancel-queued", rec, keyed.Options[msg]{})
		release := make(chan struct{})
		mustSend(t, r, "a", msg{id: 1, block: release})
		synctest.Wait()
		cancelled := r.After("a", time.Second, msg{id: 2})
		r.After("a", time.Second, msg{id: 3})
		time.Sleep(2 * time.Second) // both fired; their messages wait behind the blocked handler
		if depth := r.Snapshot().Keys[0].Depth; depth != 2 {
			t.Fatalf("depth = %d, want both fired messages queued", depth)
		}

		r.CancelTimer(cancelled)

		if depth := r.Snapshot().Keys[0].Depth; depth != 1 {
			t.Fatalf("depth = %d after the cancel, want the cancelled message gone", depth)
		}
		close(release)
		synctest.Wait()
		if got := rec.got("a"); !slices.Equal(got, []int{1, 3}) {
			t.Fatalf("handled %v, want [1 3]: a cancelled timer is never handled", got)
		}
	})
}

// A message the actor is already handling cannot be taken back; cancelling its timer then changes nothing.
func TestCancellingATimerWhoseMessageIsBeingHandledChangesNothing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](newRoot(t), "cancel-late", rec, keyed.Options[msg]{})
		release := make(chan struct{})
		id := r.After("a", time.Second, msg{id: 1, block: release})
		time.Sleep(2 * time.Second)
		synctest.Wait()

		r.CancelTimer(id)
		close(release)
		synctest.Wait()

		if got := rec.got("a"); !slices.Equal(got, []int{1}) {
			t.Fatalf("handled %v, want [1]", got)
		}
	})
}
