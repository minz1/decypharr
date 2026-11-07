package keyed_test

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

func TestATimerDeliversItsMessageThroughTheMailbox(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg](newRoot(t), "timer", rec, keyed.Options[msg]{})
		if id := r.After("a", 5*time.Second, msg{id: 1}); id == 0 {
			t.Fatal("After returned no timer id")
		}

		time.Sleep(4 * time.Second)
		synctest.Wait()
		if got := rec.got("a"); len(got) != 0 {
			t.Fatalf("handled %v before the timer was due", got)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()

		if got := rec.got("a"); !slices.Equal(got, []int{1}) {
			t.Fatalf("handled %v after the timer was due, want [1]", got)
		}
	})
}

func TestATimerSurvivesTheKeysIdleExit(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg](newRoot(t), "survive", rec, keyed.Options[msg]{IdleAfter: time.Second})
		mustSend(t, r, "a", msg{id: 1})
		r.After("a", time.Minute, msg{id: 2})

		time.Sleep(10 * time.Second)
		synctest.Wait()

		keys := r.Snapshot().Keys
		if len(keys) != 1 || keys[0].Running || keys[0].Timers != 1 {
			t.Fatalf("keys = %+v, want a: not running, one timer pending", keys)
		}
		time.Sleep(time.Minute)
		synctest.Wait()

		if got := rec.got("a"); !slices.Equal(got, []int{1, 2}) {
			t.Fatalf("handled %v, want the timer's message after the idle exit: [1 2]", got)
		}
		if keys = r.Snapshot().Keys; len(keys) != 0 {
			t.Fatalf("keys = %+v, want the key forgotten once its timer fired and it went quiet", keys)
		}
	})
}

func TestACancelledTimerNeverFires(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg](newRoot(t), "cancel", rec, keyed.Options[msg]{})
		id := r.After("a", 5*time.Second, msg{id: 1})
		keep := r.After("a", 6*time.Second, msg{id: 2})
		time.Sleep(time.Second)

		r.CancelTimer(id)
		r.CancelTimer(id) // twice is harmless
		time.Sleep(time.Minute)
		synctest.Wait()

		if got := rec.got("a"); !slices.Equal(got, []int{2}) {
			t.Fatalf("handled %v, want only the timer that was kept", got)
		}
		r.CancelTimer(keep) // after it fired is harmless too
	})
}

func TestTimersFireInDueOrder(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg](newRoot(t), "order", rec, keyed.Options[msg]{})
		r.After("a", 3*time.Second, msg{id: 3})
		r.After("a", time.Second, msg{id: 1})
		r.After("a", 2*time.Second, msg{id: 2})
		r.After("a", 2*time.Second, msg{id: 22})

		time.Sleep(time.Minute)
		synctest.Wait()

		if got := rec.got("a"); !slices.Equal(got, []int{1, 2, 22, 3}) {
			t.Fatalf("handled %v, want due order, ties in the order they were set: [1 2 22 3]", got)
		}
	})
}

func TestATimerIsNeverDroppedWhenTheMailboxIsFull(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg](newRoot(t), "timers-full", rec, keyed.Options[msg]{MailboxSize: 1})
		release := make(chan struct{})
		mustSend(t, r, "a", msg{id: 1, block: release})
		synctest.Wait()
		mustSend(t, r, "a", msg{id: 2}) // the mailbox is now full
		r.After("a", time.Second, msg{id: 3})
		r.After("a", time.Second, msg{id: 4})

		time.Sleep(2 * time.Second)
		synctest.Wait()
		close(release)
		synctest.Wait()

		if got := rec.got("a"); !slices.Equal(got, []int{1, 2, 3, 4}) {
			t.Fatalf("handled %v, want the timers queued past the bound: [1 2 3 4]", got)
		}
	})
}

func TestAHandlerSetsAndCancelsItsOwnTimers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg](newRoot(t), "own", rec, keyed.Options[msg]{})
		mustSend(t, r, "a", msg{id: 1, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			fx.After(time.Second, msg{id: 2})
			fx.CancelTimer(fx.After(time.Second, msg{id: 3}))
		}})

		time.Sleep(5 * time.Second)
		synctest.Wait()

		if got := rec.got("a"); !slices.Equal(got, []int{1, 2}) {
			t.Fatalf("handled %v, want [1 2]", got)
		}
	})
}

func TestStopCancelsTheTimers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		g := newRoot(t)
		r := keyed.New[string, msg](g, "stop-timers", rec, keyed.Options[msg]{})
		r.After("a", time.Hour, msg{id: 1})

		stop(t, g)
		time.Sleep(2 * time.Hour)
		synctest.Wait()

		if got := rec.got("a"); len(got) != 0 {
			t.Fatalf("handled %v after Stop, want nothing", got)
		}
		if id := r.After("a", time.Second, msg{id: 2}); id != 0 {
			t.Fatalf("After on a stopped runner returned timer %d, want 0", id)
		}
		if keys := r.Snapshot().Keys; len(keys) != 0 {
			t.Fatalf("keys = %+v after Stop, want none", keys)
		}
	})
}
