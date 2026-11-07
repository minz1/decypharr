package arr

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
)

// blockingCanaryService has a canary that waits until its context ends, as a probe of a stuck provider would.
func blockingCanaryService(t *testing.T, probeEnded chan<- error) *Service {
	t.Helper()
	service := New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
	service.SetQueueGuardHooks(QueueGuardHooks{
		Canary: func(ctx context.Context, _ string) CanaryVerdict {
			<-ctx.Done()
			probeEnded <- ctx.Err()
			return CanaryInconclusive
		},
	})
	for i := range 20 { // the free allowance
		if err := service.AdmitFailGrab(t.Context(), "tv", fmt.Sprintf("free-%02d", i), "realdebrid"); err != nil {
			t.Fatal(err)
		}
	}
	return service
}

// A probe that never answers is cut off, and the caller treats the provider as unproven.
func TestGuardCanaryProbeIsBoundedByATimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ended := make(chan error, 1)
		service := blockingCanaryService(t, ended)
		start := time.Now()
		err := service.AdmitFailGrab(t.Context(), "tv", "excess", "realdebrid")
		if err == nil {
			t.Fatal("a download beyond the free allowance passed on a canary that never answered")
		}
		if got := <-ended; got != context.DeadlineExceeded {
			t.Fatalf("the probe ended with %v, want its deadline", got)
		}
		if waited := time.Since(start); waited != guardCanaryTimeout {
			t.Fatalf("waited %s, want the canary timeout %s", waited, guardCanaryTimeout)
		}
	})
}

// Closing the service cancels a probe in flight, so it cannot outlive the storage it reads.
func TestGuardCanaryProbeEndsWhenTheServiceCloses(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ended := make(chan error, 1)
		service := blockingCanaryService(t, ended)
		done := make(chan error, 1)
		go func() { done <- service.AdmitFailGrab(t.Context(), "tv", "excess", "realdebrid") }()
		synctest.Wait() // the probe is running
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
		if got := <-ended; got != context.Canceled {
			t.Fatalf("the probe ended with %v, want it cancelled by Close", got)
		}
		if err := <-done; err == nil {
			t.Fatal("the caller was admitted although its probe was cancelled")
		}
	})
}
