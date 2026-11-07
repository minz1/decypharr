package repair

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
)

// An operator's force releases only a bucket waiting for a sweep or force. One a canary tripped stays tripped: the
// force is not evidence the provider works.
func TestForceDoesNotReleaseABucketTrippedByAFailingCanary(t *testing.T) {
	t.Parallel()
	service, clock := newAdmitService(t)
	service.breaker.canary = func(context.Context, Bucket, config.BreakerSettings) canaryOutcome { return canaryFail }
	// A bucket under threshold whose canary fails trips on the first Admit.
	first := HealRequest{Provider: "rd", Class: ClassHosterLink, Subject: "first"}
	var held *HeldError
	if err := service.Admit(t.Context(), first); !errors.As(err, &held) || held.Reason != string(holdCanaryFailed) {
		t.Fatalf("setup: admit = %v, want held as canary_failed", err)
	}

	forced := HealRequest{Provider: "rd", Class: ClassHosterLink, Subject: "forced", Force: true, Caller: "ops"}
	if err := service.Admit(t.Context(), forced); err != nil {
		t.Fatalf("a forced heal was refused: %v", err)
	}
	if status := service.BreakerStatus(); len(status) != 1 || status[0].Reason != string(holdCanaryFailed) {
		t.Fatalf("status = %+v, want the bucket still tripped on its failed canary", status)
	}
	clock.Advance(time.Minute)
	if err := service.Admit(t.Context(), HealRequest{
		Provider: "rd", Class: ClassHosterLink, Subject: fmt.Sprintf("next-%d", 1),
	}); !errors.As(err, &held) || held.Reason != string(holdCanaryFailed) {
		t.Fatalf("the next unforced admit = %v, want held as canary_failed", err)
	}
}
