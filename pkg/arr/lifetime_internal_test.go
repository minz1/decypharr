package arr

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
)

// A service given a lifetime ties its canary probes to it: when the owner's context ends, a probe that never
// answers is cut off, without waiting for Close or for the probe's timeout.
func TestCanaryProbeEndsWithTheServiceLifetime(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		owner, endOwner := context.WithCancel(t.Context())
		service := New(config.NewStore(&config.Config{}), nil, zerolog.Nop(), WithLifetime(owner))
		ended := make(chan error, 1)
		service.SetQueueGuardHooks(QueueGuardHooks{
			Canary: func(ctx context.Context, _ string) CanaryVerdict {
				<-ctx.Done()
				ended <- ctx.Err()
				return CanaryInconclusive
			},
		})
		for i := range 20 { // the free allowance
			if err := service.AdmitFailGrab(t.Context(), "tv", fmt.Sprintf("free-%02d", i), "realdebrid"); err != nil {
				t.Fatal(err)
			}
		}
		admitted := make(chan error, 1)
		go func() { admitted <- service.AdmitFailGrab(t.Context(), "tv", "excess", "realdebrid") }()
		synctest.Wait() // the probe is waiting

		endOwner()
		synctest.Wait()

		if got := <-ended; !errors.Is(got, context.Canceled) {
			t.Fatalf("the probe ended with %v, want the owner's cancellation", got)
		}
		if err := <-admitted; err == nil {
			t.Fatal("a download beyond the allowance passed on a probe that was cut off")
		}
	})
}
