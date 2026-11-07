package keyed_test

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

// A key's goroutine does not exit while one of its child tasks is in flight, however long that takes: the child's
// result must reach a goroutine that still holds the key's place. It exits once the child is done and the key is quiet.
func TestAKeyStaysUpWhileAChildIsInFlight(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](newRoot(t), "children", rec, keyed.Options[msg]{IdleAfter: time.Second})
		finish := make(chan struct{})
		mustSend(t, r, "a", msg{id: 1, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			_ = fx.Go("io", func(context.Context) msg {
				<-finish
				return msg{id: 2}
			})
		}})

		time.Sleep(10 * time.Second) // far past the idle time
		synctest.Wait()
		keys := r.Snapshot().Keys
		if len(keys) != 1 || !keys[0].Running || keys[0].Children != 1 {
			t.Fatalf("keys = %+v, want a: still running, one child in flight", keys)
		}

		close(finish)
		synctest.Wait()
		if got := rec.got("a"); !slices.Equal(got, []int{1, 2}) {
			t.Fatalf("handled %v, want the message and the child's result: [1 2]", got)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if keys = r.Snapshot().Keys; len(keys) != 0 {
			t.Fatalf("keys = %+v, want the key gone once the child was done and the key quiet", keys)
		}
	})
}
