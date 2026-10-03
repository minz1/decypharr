package logger_test

import (
	"io"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/logger"
)

// Reset used to replace the map field while other goroutines read it, which
// the race detector flags.
func TestRateLimitedLoggerResetIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	rl := logger.NewRateLimitedLogger(logger.WithLogger(zerolog.New(io.Discard)))
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				rl.Rate("k").Error().Msg("x")
				rl.ErrorOnce("once").Msg("x")
			}
		})
	}
	wg.Go(func() {
		for range 200 {
			rl.Reset()
		}
	})
	wg.Wait()

	rl.Reset()
	if rl.ErrorOnce("once") == nil {
		t.Fatal("key still suppressed after Reset")
	}
}
