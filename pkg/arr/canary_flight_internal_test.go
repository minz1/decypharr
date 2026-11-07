package arr

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// Callers needing the canary of one provider at the same moment share a single probe.
func TestConcurrentGuardCallersShareOneCanaryProbe(t *testing.T) {
	t.Parallel()
	service, _, _ := guardedService(t, newResearchArr())
	var probes atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	service.SetQueueGuardHooks(QueueGuardHooks{
		Canary: func(context.Context, string) CanaryVerdict {
			probes.Add(1)
			once.Do(func() { close(entered) })
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
	<-entered
	close(release)
	wg.Wait()
	if got := probes.Load(); got != 1 {
		t.Fatalf("the provider's canary was probed %d times for 8 concurrent callers, want 1", got)
	}
}
