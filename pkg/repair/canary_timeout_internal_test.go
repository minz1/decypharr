package repair

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
)

// A canary that runs out of time ends inconclusive and says so, with what it had managed: the hourly monitor can
// then tell a slow provider from a healthy one.
func TestCanaryTimeoutIsLogged(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 10, config.RepairConfig{})
	f.sweep(t.Context(), nil) // every entry is healthy and was good a day ago
	var logs bytes.Buffer
	f.service.logger = zerolog.New(&logs)
	f.client.mu.Lock()
	f.client.blockCheck = true
	f.client.mu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	settings := config.RepairConfig{}.BreakerSettings()
	if got := f.service.canaryProbe(ctx, rdHoster(), settings); got != canaryInconclusive {
		t.Fatalf("verdict = %v, want inconclusive", got)
	}
	line := logs.String()
	for _, want := range []string{
		`"level":"warn"`, "canary timed out", `"bucket":"rd/hoster_link"`, `"entries_probed":1`, `"files_checked":0`,
		`"duration"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("log %q lacks %s", line, want)
		}
	}
}

// A canary that simply fails does not log a timeout.
func TestCanaryThatFindsAFailureDoesNotLogATimeout(t *testing.T) {
	t.Parallel()
	f := newOutageFixture(t, 10, config.RepairConfig{})
	f.sweep(t.Context(), nil)
	var logs bytes.Buffer
	f.service.logger = zerolog.New(&logs)
	f.client.set(true)

	got := f.service.canaryProbe(t.Context(), rdHoster(), config.RepairConfig{}.BreakerSettings())
	if got != canaryFail || strings.Contains(logs.String(), "timed out") {
		t.Fatalf("verdict = %v, log = %q, want a plain failure", got, logs.String())
	}
}
