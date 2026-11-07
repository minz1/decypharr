package keyed_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

// askMsg is a message whose handler replies with a value, or not.
type askMsg struct {
	reply   any
	silent  bool
	panics  bool
	block   <-chan struct{}
	inside  func(ctx context.Context) error // runs in the handler; its error becomes the reply
	counted *[]int
	id      int
}

func newAskRunner(t *testing.T, opts keyed.Options[askMsg]) (*keyed.Runner[string, askMsg, struct{}], func()) {
	t.Helper()
	g := newRoot(t)
	h := keyed.HandlerFunc[string, askMsg](
		func(ctx context.Context, _ string, m askMsg, fx *keyed.Effects[string, askMsg]) {
			if m.block != nil {
				<-m.block
			}
			if m.panics {
				panic("boom")
			}
			if m.counted != nil {
				*m.counted = append(*m.counted, m.id)
			}
			if m.inside != nil {
				fx.Reply(m.inside(ctx))
				return
			}
			if !m.silent {
				fx.Reply(m.reply)
			}
		},
	)
	return keyed.New[string, askMsg, struct{}](g, "ask", h, opts), func() { stop(t, g) }
}

func TestAskReturnsTheHandlersReply(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r, _ := newAskRunner(t, keyed.Options[askMsg]{})

		got, err := r.Ask(t.Context(), "a", askMsg{reply: 42})

		if err != nil || got != 42 {
			t.Fatalf("Ask = %v, %v; want 42", got, err)
		}
	})
}

// A timed-out Ask does not mean the message failed: the actor may still handle it.
func TestAnAskTimeoutMeansTheOutcomeIsUnknown(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var handled []int
		r, _ := newAskRunner(t, keyed.Options[askMsg]{})
		release := make(chan struct{})
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		_, err := r.Ask(ctx, "a", askMsg{block: release, counted: &handled, id: 1, reply: "late"})

		if !errors.Is(err, keyed.ErrOutcomeUnknown) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Ask = %v, want ErrOutcomeUnknown wrapping the deadline", err)
		}
		close(release)
		synctest.Wait()
		if !slices.Equal(handled, []int{1}) {
			t.Fatalf("handled %v, want the message handled after the asker gave up", handled)
		}
	})
}

func TestAskFromInsideAHandlerIsRefused(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var r *keyed.Runner[string, askMsg, struct{}]
		r, _ = newAskRunner(t, keyed.Options[askMsg]{})

		got, err := r.Ask(t.Context(), "a", askMsg{inside: func(ctx context.Context) error {
			_, askErr := r.Ask(ctx, "b", askMsg{reply: 1})
			return askErr
		}})

		if err != nil {
			t.Fatal(err)
		}
		if inner, _ := got.(error); !errors.Is(inner, keyed.ErrAskInHandler) {
			t.Fatalf("the Ask inside the handler returned %v, want ErrAskInHandler", got)
		}
	})
}

func TestAskFromAChildTaskOfAHandlerIsRefused(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		g := newRoot(t)
		results := make(chan error, 1)
		var r *keyed.Runner[string, int, struct{}]
		h := keyed.HandlerFunc[string, int](func(_ context.Context, _ string, m int, fx *keyed.Effects[string, int]) {
			if m != 0 {
				return
			}
			_, _ = fx.Go("probe", func(ctx context.Context) int {
				_, err := r.Ask(ctx, "b", 1)
				results <- err
				return 1
			}, nil)
		})
		r = keyed.New[string, int, struct{}](g, "ask-child", h, keyed.Options[int]{})
		if err := r.Send("a", 0); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()

		if err := <-results; !errors.Is(err, keyed.ErrAskInHandler) {
			t.Fatalf("Ask from a child task = %v, want ErrAskInHandler", err)
		}
	})
}

func TestAskGetsAnErrorWhenTheHandlerDoesNotReply(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r, _ := newAskRunner(t, keyed.Options[askMsg]{})

		_, err := r.Ask(t.Context(), "a", askMsg{silent: true})

		if !errors.Is(err, keyed.ErrNoReply) {
			t.Fatalf("Ask = %v, want ErrNoReply", err)
		}
	})
}

func TestAskGetsAnErrorWhenTheHandlerPanics(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r, _ := newAskRunner(t, keyed.Options[askMsg]{})

		_, err := r.Ask(t.Context(), "a", askMsg{panics: true})

		if !errors.Is(err, keyed.ErrHandlerPanicked) {
			t.Fatalf("Ask = %v, want ErrHandlerPanicked", err)
		}
		if got, replyErr := r.Ask(t.Context(), "a", askMsg{reply: "after"}); replyErr != nil || got != "after" {
			t.Fatalf("Ask after the panic = %v, %v; want the actor to carry on", got, replyErr)
		}
	})
}

func TestAskAfterStopIsRefusedAndQueuedAsksAreToldTheOutcomeIsUnknown(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r, stopRunner := newAskRunner(t, keyed.Options[askMsg]{})
		release := make(chan struct{})
		go func() { _, _ = r.Ask(t.Context(), "a", askMsg{block: release}) }()
		synctest.Wait()
		queued := make(chan error, 1)
		go func() {
			_, err := r.Ask(t.Context(), "a", askMsg{reply: 1})
			queued <- err
		}()
		synctest.Wait()

		go stopRunner()
		synctest.Wait()
		close(release)
		synctest.Wait()

		if err := <-queued; !errors.Is(err, keyed.ErrOutcomeUnknown) || !errors.Is(err, keyed.ErrStopped) {
			t.Fatalf("a queued Ask at Stop = %v, want ErrOutcomeUnknown wrapping ErrStopped", err)
		}
		if _, err := r.Ask(t.Context(), "a", askMsg{reply: 2}); !errors.Is(err, keyed.ErrStopped) ||
			errors.Is(err, keyed.ErrOutcomeUnknown) {
			t.Fatalf("Ask after Stop = %v, want plain ErrStopped: nothing was queued", err)
		}
	})
}

func TestAskIsRefusedWhenTheMailboxIsFull(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r, _ := newAskRunner(t, keyed.Options[askMsg]{MailboxSize: 1})
		release := make(chan struct{})
		if err := r.Send("a", askMsg{block: release}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if err := r.Send("a", askMsg{}); err != nil {
			t.Fatal(err)
		}

		_, err := r.Ask(t.Context(), "a", askMsg{reply: 1})

		if !errors.Is(err, keyed.ErrMailboxFull) {
			t.Fatalf("Ask = %v, want ErrMailboxFull", err)
		}
		close(release)
	})
}
