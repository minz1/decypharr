package manager

import (
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
)

type warnLog struct {
	mu    sync.Mutex
	lines []string
}

func (w *warnLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if strings.Contains(string(p), `"level":"warn"`) {
		w.lines = append(w.lines, string(p))
	}
	return len(p), nil
}

func (w *warnLog) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.lines...)
}

func observedAs(phase debridTypes.Phase, raw string) entryMsg {
	m := eventMsg(grab.Event{Kind: grab.EvProviderObserved, Observed: phase, Raw: raw})
	m.seen = &pollSeen{pending: grab.PendingProvider}
	return m
}

func (h *actorHarness) poll(m entryMsg) {
	h.t.Helper()
	h.mustAsk("a", grab.Event{Kind: grab.EvPollTimer})
	h.next(grab.ChildPoll).reply <- m
	synctest.Wait()
}

func TestBackwardObservationKeepsPolling(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, time.Hour)
		warns := &warnLog{}
		h.a.logger = zerolog.New(warns)
		h.providerGrab().reply <- observedAs(debridTypes.PhaseDownloading, "")
		synctest.Wait()
		want, _ := h.diskGrab("a")
		if want.State.ProviderPhase != debridTypes.PhaseDownloading {
			t.Fatalf("provider phase %v, want downloading", want.State.ProviderPhase)
		}
		refused := h.a.refused.Load()

		h.poll(observedAs(debridTypes.PhaseRemoteQueued, ""))
		h.poll(observedAs(debridTypes.PhaseRemoteQueued, ""))
		if got := h.a.refused.Load() - refused; got != 2 {
			t.Fatalf("refused %d backward observations, want 2", got)
		}
		if got := warns.all(); len(got) != 1 || !strings.Contains(got[0], "remote_queued") {
			t.Fatalf("warnings %q, want one naming the refused edge", got)
		}
		if got, _ := h.diskGrab("a"); got.State != want.State {
			t.Fatalf("disk grab %+v after a refused observation, want %+v", got.State, want.State)
		}
		h.mustAsk("a", grab.Event{Kind: grab.EvPollTimer})
		h.next(grab.ChildPoll)

		time.Sleep(grab.PendingLimit - time.Minute)
		h.mustBe("a", grab.PhaseProvider)
		time.Sleep(time.Minute)
		h.mustBe("a", grab.PhaseFailed)
	})
}

func TestUnknownRawWarnsOnce(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		warns := &warnLog{}
		h.a.logger = zerolog.New(warns)
		h.providerGrab().reply <- observedAs(debridTypes.PhaseUnknown, "x")
		synctest.Wait()
		h.poll(observedAs(debridTypes.PhaseUnknown, "x"))
		h.poll(observedAs(debridTypes.PhaseUnknown, "y"))
		h.poll(observedAs(debridTypes.PhaseUnknown, "x"))
		got := warns.all()
		if len(got) != 2 || !strings.Contains(got[0], `"x"`) || !strings.Contains(got[1], `"y"`) {
			t.Fatalf("warnings %q, want one for x then one for y", got)
		}
	})
}
