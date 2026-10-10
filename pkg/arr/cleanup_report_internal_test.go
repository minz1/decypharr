package arr

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// An arr that stays down logs one ERROR, not one per 10s cleanup tick, and
// its recovery is logged once.
func TestReportCleanupLogsStateChangesOnly(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	service := testService(Arr{})
	service.logger = zerolog.New(&out).Level(zerolog.InfoLevel)

	down := errors.New("connection refused")
	for range 5 {
		service.reportCleanup("arr", down)
	}
	service.reportCleanup("arr", nil)
	service.reportCleanup("arr", nil)

	logged := out.String()
	if n := strings.Count(logged, `"level":"error"`); n != 1 {
		t.Errorf("logged %d errors for one outage, want 1:\n%s", n, logged)
	}
	if n := strings.Count(logged, `"level":"info"`); n != 1 {
		t.Errorf("logged %d recoveries, want 1:\n%s", n, logged)
	}
}
