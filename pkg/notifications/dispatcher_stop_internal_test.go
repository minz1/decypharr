package notifications

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

// stopOnHandler puts the dispatcher behind a test hook: the first message of
// the armed kind stops the runner's group, and the dispatcher handles it only
// once the handler's ctx has ended, so it runs on a stopping runner that
// refuses fx.Go and fx.After. It records the phase each Handle leaves.
type stopOnHandler struct {
	d     *dispatcher
	group *task.Group
	t     *testing.T

	mu      sync.Mutex
	armed   msgKind
	stopped chan struct{}
	phases  []phase
}

func (h *stopOnHandler) Activate(ctx context.Context, key string) notifierState {
	return h.d.Activate(ctx, key)
}

func (h *stopOnHandler) Handle(
	ctx context.Context,
	key string,
	st *notifierState,
	m msg,
	fx *keyed.Effects[string, msg],
) {
	h.mu.Lock()
	fire := h.armed != 0 && m.kind == h.armed
	if fire {
		h.armed = 0
	}
	h.mu.Unlock()
	if fire {
		go func() {
			stopGroup(h.t, h.group)
			close(h.stopped)
		}()
		<-ctx.Done()
	}
	h.d.Handle(ctx, key, st, m, fx)
	h.mu.Lock()
	h.phases = append(h.phases, st.phase)
	h.mu.Unlock()
}

func (h *stopOnHandler) arm(kind msgKind) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.armed = kind
}

func (h *stopOnHandler) lastPhase() phase {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.phases) == 0 {
		return 0
	}
	return h.phases[len(h.phases)-1]
}

// newStopRig is newRig with the dispatcher behind a stopOnHandler.
func newStopRig(t *testing.T, notifiers ...Notifier) (*rig, *stopOnHandler) {
	t.Helper()
	g := task.NewRoot(context.WithoutCancel(t.Context()), "test", zerolog.Nop())
	t.Cleanup(func() { stopGroup(t, g) })
	logs := &logBuffer{}
	obs := newRecordObserver()
	logger := zerolog.New(logs).Level(zerolog.TraceLevel)
	d := &dispatcher{notifiers: map[string]Notifier{}, logger: logger, obs: obs}
	for _, n := range notifiers {
		d.notifiers[n.Name()] = n
	}
	h := &stopOnHandler{d: d, group: g, t: t, stopped: make(chan struct{})}
	runner := keyed.New(g, "dispatch", h, keyed.Options[msg]{
		MailboxSize: mailboxSize,
		IdleAfter:   neverIdle,
		Logger:      logger,
	})
	return &rig{group: g, runner: runner, obs: obs, logs: logs}, h
}

// checkStopped holds a stopped notifier to its end state: every event dropped
// as stopped exactly once, in order, none delivered, the phase left Idle and
// no illegal phase change logged.
func checkStopped(t *testing.T, r *rig, h *stopOnHandler, wantDrops []dropRecord) {
	t.Helper()
	if got := r.obs.gotDrops("hook"); !slices.Equal(got, wantDrops) {
		t.Errorf("drops %v, want %v", got, wantDrops)
	}
	if got := r.obs.gotDelivered("hook"); len(got) != 0 {
		t.Errorf("delivered %v, want none", got)
	}
	if got := h.lastPhase(); got != phaseIdle {
		t.Errorf("phase after the stop = %v, want idle", got)
	}
	if n := r.logs.count("error", ""); n != 0 {
		t.Errorf("%d ERROR lines, want 0", n)
	}
}

// TestStoppingRunnerRefusesTheFirstSend: an event that reaches an idle
// notifier while the runner stops is dropped as stopped with no send.
func TestStoppingRunnerRefusesTheFirstSend(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFake("hook")
		r, h := newStopRig(t, f)
		h.arm(msgEnqueue)
		r.notify(t, "hook", 1)
		<-h.stopped
		synctest.Wait()
		if n := len(f.sent()); n != 0 {
			t.Fatalf("sends = %d, want 0", n)
		}
		checkStopped(t, r, h, []dropRecord{{id: "1", reason: dropStopped, attempts: 0}})
	})
}

// TestStoppingRunnerRefusesTheRetryTimer: a retryable failure handled while
// the runner stops gets no timer (fx.After returns 0), so the head and every
// queued event end as stopped.
func TestStoppingRunnerRefusesTheRetryTimer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		hang := make(chan struct{})
		f := newFake("hook", outcome{hang: hang, err: errNetwork})
		r, h := newStopRig(t, f)
		r.notify(t, "hook", 1)
		r.notify(t, "hook", 2)
		r.notify(t, "hook", 3)
		synctest.Wait()
		h.arm(msgSendDone)
		close(hang)
		<-h.stopped
		synctest.Wait()
		if n := len(f.sent()); n != 1 {
			t.Fatalf("sends = %d, want 1", n)
		}
		checkStopped(t, r, h, []dropRecord{
			{id: "1", reason: dropStopped, attempts: 1},
			{id: "2", reason: dropStopped, attempts: 0},
			{id: "3", reason: dropStopped, attempts: 0},
		})
	})
}

// TestStoppingRunnerRefusesTheRetrySend: a retry timer handled while the
// runner stops cannot start the send; the head ends as stopped, the phase
// goes Waiting to Idle, and the queue drains as stopped.
func TestStoppingRunnerRefusesTheRetrySend(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFake("hook", outcome{err: status(http.StatusServiceUnavailable, 0)})
		r, h := newStopRig(t, f)
		r.notify(t, "hook", 1)
		r.notify(t, "hook", 2)
		r.notify(t, "hook", 3)
		synctest.Wait()
		if got := h.lastPhase(); got != phaseWaiting {
			t.Fatalf("phase before the retry = %v, want waiting", got)
		}
		h.arm(msgRetryDue)
		time.Sleep(time.Second)
		<-h.stopped
		synctest.Wait()
		if n := len(f.sent()); n != 1 {
			t.Fatalf("sends = %d, want 1", n)
		}
		checkStopped(t, r, h, []dropRecord{
			{id: "1", reason: dropStopped, attempts: 1},
			{id: "2", reason: dropStopped, attempts: 0},
			{id: "3", reason: dropStopped, attempts: 0},
		})
	})
}
