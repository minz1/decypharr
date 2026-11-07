package keyed_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

// An Ask whose context has already ended sends nothing, and says so: "not sent" is a known outcome, unlike the
// "unknown" of a timeout after the message was queued.
func TestAnAskWithAnEndedContextIsNotSent(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var handled []int
		r, _ := newAskRunner(t, keyed.Options[askMsg]{})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := r.Ask(ctx, "a", askMsg{counted: &handled, id: 1, reply: "x"})

		if !errors.Is(err, keyed.ErrNotSent) || !errors.Is(err, context.Canceled) {
			t.Fatalf("Ask = %v, want ErrNotSent wrapping the cancellation", err)
		}
		if errors.Is(err, keyed.ErrOutcomeUnknown) {
			t.Fatalf("Ask = %v: nothing was queued, so the outcome is known", err)
		}
		synctest.Wait()
		if len(handled) != 0 {
			t.Fatalf("handled %v, want the message never queued", handled)
		}
		if info := r.Snapshot(); len(info.Keys) != 0 {
			t.Fatalf("keys = %+v, want none: nothing was queued", info.Keys)
		}
	})
}
