package repair

import (
	"context"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/arr"
)

// generationDrain is cmd/decypharr's generationDrainDeadline: a restart drains the whole generation within it, or
// the process exits instead of building the next generation.
const generationDrain = 25 * time.Second

// A config-save restart that lands while a canary probe waits on a provider that never answers still drains in
// time: stopping repair cancels the probe, the probe's provider call returns on that cancellation, and the group is
// done well within the generation's drain deadline, so the restart is never turned into a process exit. (A probe
// that ignored its cancellation would hold the group to the deadline; cmd/decypharr then exits on purpose rather
// than start a generation beside it, see TestStopTimeoutDoesNotStartTheNextGeneration.)
func TestRestartDrainEndsAProbeWaitingOnTheProvider(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 1, config.RepairConfig{})
	f.sweep(t.Context(), nil) // the entry is healthy and was good a day ago, so it is a canary candidate
	f.client.mu.Lock()
	f.client.blockCheck = true
	f.client.entered = make(chan struct{}, 4)
	entered := f.client.entered
	f.client.mu.Unlock()

	verdict := make(chan arr.CanaryVerdict, 1)
	go func() { verdict <- f.service.QueueCanary(context.WithoutCancel(t.Context()), "rd") }()
	select {
	case <-entered: // the probe is waiting on the provider
	case v := <-verdict:
		t.Fatalf("the canary returned %v without asking the provider", v)
	case <-time.After(generationDrain):
		t.Fatal("the canary never asked the provider")
	}

	begin := time.Now()
	f.service.Stop()
	ctx, cancel := task.Detached(t.Context(), generationDrain)
	defer cancel()
	if err := f.g.Stop(ctx); err != nil {
		t.Fatalf("the repair group did not drain: %v", err)
	}
	if took := time.Since(begin); took >= generationDrain {
		t.Fatalf("the drain took %s, want well under the %s generation deadline", took, generationDrain)
	}
	if v := <-verdict; v == arr.CanaryPass || v == arr.CanaryFail {
		t.Fatalf("a probe cut short by the stop reported %v, want an open verdict", v)
	}
}
