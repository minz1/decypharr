package arr

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
)

// Callers needing the canary of one provider at the same moment share a single probe. Run in a synctest bubble,
// every caller is blocked inside the probe or waiting on its flight before the probe is allowed to finish, so the
// count does not depend on scheduling and is 8 without the flight.
func TestConcurrentGuardCallersShareOneCanaryProbe(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		service := New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
		clock := &fakeClock{now: time.Unix(1_000_000, 0)}
		service.now = clock.Now
		var probes atomic.Int64
		release := make(chan struct{})
		service.SetQueueGuardHooks(QueueGuardHooks{
			Canary: func(context.Context, string) CanaryVerdict {
				probes.Add(1)
				<-release
				return CanaryPass
			},
		})
		for i := range 20 { // the free allowance
			if err := service.AdmitFailGrab(t.Context(), "tv", fmt.Sprintf("free-%02d", i), "realdebrid"); err != nil {
				t.Fatal(err)
			}
		}

		var wg sync.WaitGroup
		for i := range 8 {
			wg.Go(func() {
				_ = service.AdmitFailGrab(t.Context(), "tv", fmt.Sprintf("excess-%02d", i), "realdebrid")
			})
		}
		synctest.Wait()
		if got := probes.Load(); got != 1 {
			t.Fatalf("the provider's canary is being probed %d times by 8 concurrent callers, want 1", got)
		}
		close(release)
		wg.Wait()
		if got := probes.Load(); got != 1 {
			t.Fatalf("the provider's canary was probed %d times, want 1", got)
		}
	})
}
