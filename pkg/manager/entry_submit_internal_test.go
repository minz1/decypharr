package manager

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/task/keyed"
	"github.com/sirrobot01/decypharr/internal/utils"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func addReq() *ImportRequest {
	return &ImportRequest{Magnet: &utils.Magnet{InfoHash: "a", Name: "a"}}
}

func submitCount(h *actorHarness) int {
	return len(slices.DeleteFunc(h.logged(), func(s string) bool { return s != "start:submit" }))
}

func failedState() grab.State {
	return grab.State{Phase: grab.PhaseFailed, Failure: grab.Failure{Reason: "refused", At: time.Now()}}
}

func TestReAddOfAFailedHashSubmitsExactlyOnce(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		m := h.manager()
		seed(t, store, testEntry("a"), failedState())
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})

		for range 2 {
			if err := m.submitEntry(t.Context(), testEntry("a"), addReq()); err != nil {
				t.Fatal(err)
			}
		}
		h.next(grab.ChildSubmit)
		h.none()
		if got := submitCount(h); got != 1 {
			t.Fatalf("submit children started = %d, want 1", got)
		}
		if got := h.phase("a"); got != grab.PhaseSubmitting {
			t.Fatalf("phase %v, want submitting", got)
		}
	})
}

func TestDuplicateAddOfAnActiveHashSkipsTheProvider(t *testing.T) {
	t.Parallel()
	for _, phase := range []grab.Phase{grab.PhaseProvider, grab.PhaseAwaitingLinks, grab.PhaseActing} {
		t.Run(phase.String(), func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			synctest.Test(t, func(t *testing.T) {
				h := newActorHarness(t, store)
				seedPhase(t, store, "a", phase)
				h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
				synctest.Wait()
				<-h.started

				if err := h.manager().submitEntry(t.Context(), testEntry("a"), addReq()); err != nil {
					t.Fatal(err)
				}
				h.none()
				if got := submitCount(h); got != 0 {
					t.Fatalf("submit children started = %d, want none", got)
				}
				if got := h.phase("a"); got != phase {
					t.Fatalf("phase %v after a duplicate add, want %v", got, phase)
				}
			})
		})
	}
}

func TestConcurrentAddsSubmitOnce(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		m := h.manager()
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for range 8 {
			wg.Go(func() {
				errs <- m.submitEntry(t.Context(), testEntry("a"), addReq())
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		h.next(grab.ChildSubmit)
		h.none()
		if got := submitCount(h); got != 1 {
			t.Fatalf("submit children started = %d, want 1", got)
		}
	})
}

func TestSubmittingIsPersistedBeforeThePost(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		if err := h.manager().submitEntry(t.Context(), testEntry("a"), addReq()); err != nil {
			t.Fatal(err)
		}
		run := h.next(grab.ChildSubmit)
		if got := h.diskPhase(); got != grab.PhaseSubmitting {
			t.Fatalf("disk phase %v while the submit runs, want submitting", got)
		}
		if run.call.grab.State.Phase != grab.PhaseSubmitting {
			t.Fatalf("submit child started in phase %v, want submitting", run.call.grab.State.Phase)
		}
	})
}

func TestThrottledGrabWaitsForSlotFreed(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		ev := grab.Event{Kind: grab.EvSubmitResult, Outcome: grab.OutcomeThrottled}
		h.submitting("a").reply <- entryMsg{ev: &ev, providers: []string{"torbox"}}
		synctest.Wait()
		if got := h.phase("a"); got != grab.PhaseThrottled {
			t.Fatalf("phase %v, want throttled", got)
		}

		h.a.admit.SlotFreed("realdebrid")
		h.none()
		h.manager().slotFreed("torbox")
		h.next(grab.ChildSubmit)
		if got := h.phase("a"); got != grab.PhaseSubmitting {
			t.Fatalf("phase %v after the slot freed, want submitting", got)
		}
	})
}

func TestThrottledGrabRetriesOnItsTimer(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		reply(h.submitting("a"), grab.Event{Kind: grab.EvSubmitResult, Outcome: grab.OutcomeThrottled})
		synctest.Wait()
		if got := h.phase("a"); got != grab.PhaseThrottled {
			t.Fatalf("phase %v, want throttled", got)
		}
		h.none()

		time.Sleep(throttledRetryDelay(1) - time.Second)
		h.none()
		time.Sleep(time.Second)
		h.next(grab.ChildSubmit)
	})
}

func stoppedManager(t *testing.T) *Manager {
	t.Helper()
	root := task.NewRoot(context.WithoutCancel(t.Context()), "test", zerolog.Nop())
	grabs := &grabIndex{}
	a := newEntryActor(root, entryDeps{
		store:  newTestStore(t),
		grabs:  grabs,
		logger: zerolog.Nop(), capacity: 1,
		child: func(entryChildCall) func(context.Context) entryMsg {
			return func(context.Context) entryMsg { return entryMsg{} }
		},
		publish:    func(*storage.Entry) error { return nil },
		fail:       func(grab.Grab, grab.FailEffect) {},
		packOf:     func() map[string]string { return nil },
		inLibrary:  func(string) bool { return false },
		pollEvery:  testPollEvery,
		retryDelay: throttledRetryDelay,
	})
	ctx, cancel := task.Detached(t.Context(), time.Minute)
	defer cancel()
	if err := root.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	return &Manager{entries: a, grabs: grabs, logger: zerolog.Nop()}
}

func TestAddDuringStopReturnsRetryable(t *testing.T) {
	t.Parallel()
	err := stoppedManager(t).submitEntry(t.Context(), testEntry("a"), addReq())
	typed, ok := errors.AsType[*customerror.Error](err)
	if !ok || typed.StatusCode() != http.StatusServiceUnavailable || !typed.IsRetryable() {
		t.Fatalf("add during a stop = %v, want a retryable 503", err)
	}
	if !errors.Is(err, keyed.ErrStopped) {
		t.Fatalf("add during a stop = %v, want the stop as its cause", err)
	}
}

func TestReAddKeepsAddedOnAndProviders(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		old := testEntry("a")
		old.AddedOn = time.Now().Add(-48 * time.Hour).Truncate(time.Second)
		old.Providers = map[string]*storage.ProviderEntry{
			"torbox": {Provider: "torbox", ID: "tb1", Status: debridTypes.TorrentStatusError},
		}
		seed(t, store, old, failedState())
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})

		fresh := testEntry("a")
		fresh.AddedOn = time.Now()
		if err := h.manager().submitEntry(t.Context(), fresh, addReq()); err != nil {
			t.Fatal(err)
		}
		h.next(grab.ChildSubmit)
		got, err := store.GetQueuedGrab("a")
		if err != nil {
			t.Fatal(err)
		}
		if !got.AddedOn.Equal(old.AddedOn) {
			t.Fatalf("AddedOn = %v after a re-add, want %v", got.AddedOn, old.AddedOn)
		}
		if p := got.Providers["torbox"]; p == nil || p.ID != "tb1" {
			t.Fatalf("providers = %+v after a re-add, want the failed grab's placement kept", got.Providers)
		}
	})
}

func TestAddErrorMapsEveryRefusal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		err         error
		wantStatus  int
		wantRetry   bool
		wantHoldsOn bool
	}{
		{"stopped", keyed.ErrStopped, http.StatusServiceUnavailable, true, false},
		{"mailbox full", keyed.ErrMailboxFull, http.StatusServiceUnavailable, true, false},
		{"being deleted", &grab.RefusedError{Phase: grab.PhaseDeleting}, http.StatusServiceUnavailable, true, false},
		{"season child", &grab.RefusedError{Phase: grab.PhaseFailed}, http.StatusConflict, false, false},
		{"not accepted", &grab.RefusedError{Phase: grab.PhaseProvider}, http.StatusConflict, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := addError(tc.err)
			typed, ok := errors.AsType[*customerror.Error](got)
			if !ok || typed.StatusCode() != tc.wantStatus || typed.IsRetryable() != tc.wantRetry {
				t.Fatalf("addError(%v) = %v, want status %d retryable=%v", tc.err, got, tc.wantStatus, tc.wantRetry)
			}
			if mayHoldRecord(got) != tc.wantHoldsOn {
				t.Fatalf("mayHoldRecord(%v) = %v, want %v", got, !tc.wantHoldsOn, tc.wantHoldsOn)
			}
		})
	}
	if addError(nil) != nil || !mayHoldRecord(nil) {
		t.Fatal("a successful add must map to nil and may hold its record")
	}
	other := errors.New("save failed")
	if got := addError(other); !errors.Is(got, other) || !mayHoldRecord(got) {
		t.Fatalf("addError(other) = %v, holds = %v; want it unchanged and possibly holding the record", got,
			mayHoldRecord(got))
	}
}

func TestRequestFromEntryKeepsManual(t *testing.T) {
	t.Parallel()
	m := withTestConfig(t, &Manager{logger: zerolog.Nop()})
	e := testEntry("a")
	e.Manual = true
	if req := m.requestFromEntry(e); !req.Manual {
		t.Fatal("a request rebuilt from a manual entry lost Manual")
	}
}
