package types_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// Wait sleeps for d, or returns ctx's error as soon as ctx ends.
func TestWaitReturnsAtCancel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		time.AfterFunc(time.Second, cancel)
		var o types.ProviderOptions
		start := time.Now()
		if err := o.Wait(ctx, time.Hour); !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait err = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Fatalf("Wait returned after %s, want 1s", elapsed)
		}
	})
}

// Wait sleeps the full d when ctx stays live.
func TestWaitTimesOutWhileLive(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var o types.ProviderOptions
		start := time.Now()
		if err := o.Wait(t.Context(), time.Minute); err != nil {
			t.Fatalf("Wait err = %v, want nil", err)
		}
		if elapsed := time.Since(start); elapsed != time.Minute {
			t.Fatalf("Wait returned after %s, want 1m", elapsed)
		}
	})
}
