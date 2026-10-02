package logger_test

import (
	"io"
	"os"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
)

// NewRateLimitedLogger builds the default logger, which loads the config and
// creates the log directory, so point both at a throwaway directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "decypharr-logger-test-")
	if err != nil {
		panic(err)
	}
	config.SetConfigPath(dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

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
