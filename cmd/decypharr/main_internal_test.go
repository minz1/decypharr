package decypharr

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
)

// A restart whose configuration does not load leaves the running generation
// serving; the process used to exit and stay down.
func TestRestartWithUnloadableConfigKeepsServing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		restartCh := make(chan struct{}, 1)
		serviceResult := make(chan error, 1)
		loads := 0
		reload := func() (*config.Config, error) {
			loads++
			if loads == 1 {
				return nil, errors.New(`shared_dir_mode "775x" is not an octal file mode`)
			}
			return config.New(t.TempDir()), nil
		}

		type result struct {
			end  endKind
			next *config.Config
		}
		done := make(chan result, 1)
		go func() {
			end, next, _ := awaitEnd(ctx, restartCh, serviceResult, reload, zerolog.Nop())
			done <- result{end, next}
		}()

		restartCh <- struct{}{}
		synctest.Wait()
		select {
		case r := <-done:
			t.Fatalf("generation ended (%v) after a restart whose config failed to load", r.end)
		default:
		}

		restartCh <- struct{}{}
		r := <-done
		if r.end != endRestart || r.next == nil {
			t.Fatalf("end = %v, next = %v; want a restart with the loaded config", r.end, r.next)
		}
	})
}
