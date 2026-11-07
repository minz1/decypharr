package keyed_test

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

// The runner is stopped only when nothing of it is left: not its key goroutines, and not a child task that is
// still running, however the key's goroutine ended.
func TestTheRunnerIsStoppingWhileAChildTaskStillRuns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		g := newRoot(t)
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](g, "kids", rec, keyed.Options[msg]{})
		release := make(chan struct{})
		mustSend(t, r, "a", msg{id: 1, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			_ = fx.Go("io", func(context.Context) msg { // ignores its context, as provider calls do
				<-release
				return msg{id: 2}
			}, nil)
		}})
		synctest.Wait()

		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			stop(t, g)
		}()
		synctest.Wait() // the key's goroutine has exited; the child has not

		if got := r.Snapshot().State; got != "stopping" {
			t.Fatalf("state = %q while a child task runs, want stopping", got)
		}
		close(release)
		<-stopped
		if got := r.Snapshot().State; got != "stopped" {
			t.Fatalf("state = %q after the child ended, want stopped", got)
		}
	})
}
