package decypharr

import (
	"context"
	"errors"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
)

// When the manager fails to start, the services created after it find the root already stopped and say so with a
// bare ErrStopped. The failure that stopped the root is the one reported, not that echo.
func TestBuildFailureReportsTheRootFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	failure := errors.New("start: the Arr indexer is closed")
	sup := h.supervisor(func(gen *generation, cfg *config.Config, _ *logger.Factory) (func(), error) {
		h.record(cfg, gen)
		if err := gen.root.Child("manager").
			GoCritical("start", func(context.Context) error { return failure }); err != nil {
			return noop, err
		}
		<-gen.root.Failed()
		// The next service finds the root stopped.
		return noop, gen.root.Child("http").Go("serve", func(context.Context) error { return nil })
	})

	err := awaitRun(t, h.runAsync(t.Context(), sup))

	if !errors.Is(err, failure) {
		t.Fatalf("run returned %v, want the failure that stopped the root", err)
	}
}
