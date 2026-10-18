package manager

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/customerror"
)

// A provider at its active-download cap was retried every 30s forever; back off up to 10 minutes.
func TestThrottledRetryBacksOff(t *testing.T) {
	t.Parallel()
	for attempts, want := range map[int]time.Duration{
		0: 30 * time.Second, 1: time.Minute, 2: 2 * time.Minute, 4: 8 * time.Minute, 5: 10 * time.Minute, 60: 10 * time.Minute,
	} {
		if got := throttledRetryDelay(attempts); got != want {
			t.Errorf("throttledRetryDelay(%d) = %s, want %s", attempts, got, want)
		}
	}
}

func TestThrottledJobCountsItsRetries(t *testing.T) {
	t.Parallel()
	m := withTestConfig(t, &Manager{logger: zerolog.Nop()})
	m.jobQueue = NewJobQueue(t.Context(), 1, func(context.Context, *Job) {}, zerolog.Nop())
	t.Cleanup(m.jobQueue.Close)
	job := &Job{ID: "job", Type: JobTypeTorrent}
	for range 3 {
		m.handleJobError(job, fmt.Errorf("submit: %w", customerror.ErrTooManyActiveDownloads))
	}
	if job.ThrottledRetries != 3 {
		t.Fatalf("ThrottledRetries = %d, want 3", job.ThrottledRetries)
	}
}
