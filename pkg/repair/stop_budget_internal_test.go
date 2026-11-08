package repair

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
)

// Stop only cancels: a probe that ignores its cancellation does not hold it, the group's own stop waits for it.
func TestStopDoesNotWait(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		service := New(testGroup(t), Dependencies{})
		block := make(chan struct{})
		release := sync.OnceFunc(func() { close(block) })
		defer release() // a failed assertion must not leave the probe blocked, or synctest hides it behind a deadlock
		started := make(chan struct{})
		service.breaker.canary = func(context.Context, Bucket, config.BreakerSettings) canaryOutcome {
			close(started)
			<-block // a probe that ignores its cancellation
			return canaryInconclusive
		}
		go service.QueueCanary(t.Context(), "rd")
		<-started

		begin := time.Now()
		service.Stop()
		if waited := time.Since(begin); waited != 0 {
			t.Fatalf("Stop waited %s with a stuck probe, want no wait", waited)
		}
		release()
		synctest.Wait()
	})
}

// A sweep or probe that ignores cancellation shows in the group's stop-timeout task list, and Stop logs no private
// drain warning about it.
func TestStopTimeoutListsLiveSweep(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		g := task.NewRoot(t.Context(), "test", zerolog.New(&logs))
		service := New(g, Dependencies{Logger: zerolog.New(&logs)})
		block := make(chan struct{})
		release := sync.OnceFunc(func() { close(block) })
		defer release() // a failed assertion must not leave the sweep blocked, or synctest hides it behind a deadlock
		started := make(chan struct{})
		if err := g.Go("sweep run-1", func(context.Context) error {
			close(started)
			<-block // a sweep that ignores its cancellation
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		<-started

		service.Stop()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		err := g.Stop(ctx)
		if !errors.Is(err, task.ErrStopTimeout) || !strings.Contains(err.Error(), "sweep run-1") {
			t.Fatalf("group stop = %v, want a stop timeout naming the live sweep", err)
		}
		if strings.Contains(logs.String(), "drain timed out") ||
			strings.Contains(logs.String(), "did not stop in time") {
			t.Fatalf("logs %q carry a private drain warning", logs.String())
		}
		release()
		synctest.Wait()
	})
}
