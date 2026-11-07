package repair

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
)

// A held heal is retried by its caller; retrying must not keep the bucket tripped.
func TestRetriesOfHeldSubjectsDoNotKeepTheBucketTripped(t *testing.T) {
	t.Parallel()
	service, clock := newAdmitService(t)
	service.breaker.canary = func(context.Context, Bucket, config.BreakerSettings) canaryOutcome { return canaryPass }
	retry := func() (admitted int) {
		for i := range 10 {
			err := service.Admit(t.Context(), HealRequest{
				Provider: "rd", Class: ClassDropped, Subject: fmt.Sprintf("entry-%d", i),
			})
			if err == nil {
				admitted++
			}
		}
		return admitted
	}

	if got := retry(); got != 9 {
		t.Fatalf("first pass admitted %d, want 9 before the tenth distinct entry trips the bucket", got)
	}
	released := time.Duration(-1)
	for elapsed := 5 * time.Minute; elapsed <= 2*time.Hour; elapsed += 5 * time.Minute {
		clock.Advance(5 * time.Minute)
		if got := retry(); got > 0 {
			if got != 10 {
				t.Fatalf("at %s %d of 10 were admitted, want all of them once the canary passes", elapsed, got)
			}
			released = elapsed
			break
		}
	}
	if released < 0 || released > 65*time.Minute {
		t.Fatalf("released after %s of retries, want within just over the hour of the window", released)
	}
	if len(service.BreakerStatus()) != 0 {
		t.Fatalf("bucket still open: %+v", service.BreakerStatus())
	}
}
