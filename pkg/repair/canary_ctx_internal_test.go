package repair

import (
	"context"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

// A canary whose context has already ended reads no storage: it is inconclusive, not "nothing to probe".
func TestCanaryChecksItsContextBeforeReadingStorage(t *testing.T) {
	t.Parallel()
	service := New(testGroup(t), Dependencies{}) // no storage at all
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got := service.canaryProbe(ctx, rdHoster(), config.RepairConfig{}.BreakerSettings())
	if got != canaryInconclusive {
		t.Fatalf("verdict = %v, want inconclusive: the context ended before anything was read", got)
	}
}
