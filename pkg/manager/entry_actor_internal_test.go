package manager

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const testPollEvery = 30 * time.Second

type childRun struct {
	call  entryChildCall
	reply chan entryMsg
}

type actorHarness struct {
	t       *testing.T
	store   *storage.Storage
	grabs   *grabIndex
	a       *entryActor
	started chan childRun
	mu      sync.Mutex
	log     []string
	fails   []grab.FailEffect
	root    *task.Group
	down    bool
	holds   []string
	holdErr error
}

func newTestStore(t *testing.T) *storage.Storage {
	t.Helper()
	s, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newActorHarness(t *testing.T, store *storage.Storage) *actorHarness {
	t.Helper()
	h := &actorHarness{t: t, store: store, grabs: &grabIndex{}, started: make(chan childRun, 256)}
	root := task.NewRoot(context.WithoutCancel(t.Context()), "test", zerolog.Nop())
	h.root = root
	t.Cleanup(func() {
		if !h.down {
			h.stop()
		}
	})
	h.a = newEntryActor(root, entryDeps{
		store:    store,
		grabs:    h.grabs,
		logger:   zerolog.Nop(),
		capacity: 1,
		child:    h.child,
		publish: func(e *storage.Entry) error {
			h.record("publish")
			return store.PublishEntry(e.Clone())
		},
		fail: func(_ grab.Grab, effect grab.FailEffect) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.fails = append(h.fails, effect)
		},
		packOf:     func() map[string]string { return nil },
		inLibrary:  func(string) bool { return false },
		pollEvery:  testPollEvery,
		retryDelay: throttledRetryDelay,
		completed:  func(grab.Grab) {},
		hold: func(provider, id, key string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.holdErr != nil {
				return h.holdErr
			}
			h.holds = append(h.holds, "hold:"+provider+"/"+id+"/"+key)
			return nil
		},
		unhold: func(provider, id, key string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.holdErr != nil {
				return h.holdErr
			}
			h.holds = append(h.holds, "unhold:"+provider+"/"+id+"/"+key)
			return nil
		},
	})
	return h
}

func (h *actorHarness) held() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.holds)
}

func (h *actorHarness) record(s string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log = append(h.log, s)
}

func (h *actorHarness) logged() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.log)
}

func (h *actorHarness) child(call entryChildCall) func(ctx context.Context) entryMsg {
	h.record("start:" + childKind(call.kind).String())
	return func(ctx context.Context) entryMsg {
		run := childRun{call: call, reply: make(chan entryMsg, 1)}
		h.started <- run
		select {
		case m := <-run.reply:
			return m
		case <-ctx.Done():
			return entryMsg{}
		}
	}
}

// next returns the child started since the last call; it fails the test if none or another kind started.
func (h *actorHarness) next(kind grab.ChildKind) childRun {
	h.t.Helper()
	synctest.Wait()
	select {
	case run := <-h.started:
		if run.call.kind != kind {
			h.t.Fatalf("started %s, want %s", childKind(run.call.kind), childKind(kind))
		}
		return run
	default:
		h.t.Fatalf("no %s child started", childKind(kind))
	}
	return childRun{}
}

// none fails the test if a child started since the last call.
func (h *actorHarness) none() {
	h.t.Helper()
	synctest.Wait()
	select {
	case run := <-h.started:
		h.t.Fatalf("started %s, want none", childKind(run.call.kind))
	default:
	}
}

func (h *actorHarness) ask(key string, m entryMsg) error {
	h.t.Helper()
	v, err := h.a.runner.Ask(h.t.Context(), key, m)
	if err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	return v.(error)
}

func (h *actorHarness) mustAsk(key string, ev grab.Event) {
	h.t.Helper()
	if err := h.ask(key, eventMsg(ev)); err != nil {
		h.t.Fatalf("%s: %v", ev.Kind, err)
	}
}

func (h *actorHarness) send(m entryMsg) {
	h.t.Helper()
	if err := h.a.runner.Send("a", m); err != nil {
		h.t.Fatal(err)
	}
	synctest.Wait()
}

func (h *actorHarness) phase(key string) grab.Phase {
	h.t.Helper()
	v, ok := h.grabs.get(key)
	if !ok {
		return grab.PhaseAbsent
	}
	return v.State.Phase
}

// diskPhase is the phase the record of "a" on disk loads as.
func (h *actorHarness) diskPhase() grab.Phase {
	h.t.Helper()
	e, err := h.store.GetQueuedGrab("a")
	if errors.Is(err, appendstore.ErrKeyNotFound) {
		return grab.PhaseAbsent
	}
	if err != nil {
		h.t.Fatal(err)
	}
	g, err := grab.Load(e, nil, false)
	if err != nil {
		h.t.Fatal(err)
	}
	return g.State.Phase
}

func testEntry(key string) *storage.Entry {
	return &storage.Entry{
		InfoHash: key, Name: key, Category: "sonarr", Action: config.DownloadActionSymlink,
		Files: map[string]*storage.File{},
	}
}

// seed writes key's record in phase, as the actor would have.
func seed(t *testing.T, store *storage.Storage, e *storage.Entry, st grab.State) {
	t.Helper()
	st.Key = strings.ToLower(e.InfoHash)
	if st.PhaseSince.IsZero() {
		st.PhaseSince = time.Now()
	}
	switch st.Phase {
	case grab.PhaseProvider, grab.PhaseAwaitingLinks, grab.PhaseActing, grab.PhaseComplete:
		if e.Providers == nil {
			e.Providers = map[string]*storage.ProviderEntry{}
		}
		if _, ok := e.Providers["realdebrid"]; !ok {
			e.Providers["realdebrid"] = &storage.ProviderEntry{
				Provider: "realdebrid", Status: debridTypes.TorrentStatusDownloaded,
			}
		}
		e.SetActive("realdebrid", true)
	case grab.PhaseAbsent, grab.PhaseQueued, grab.PhaseThrottled, grab.PhaseSubmitting, grab.PhaseFailed,
		grab.PhaseDeleting:
	}
	if err := grab.Encode(grab.Grab{State: st, Entry: e}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateQueuedGrab(e); err != nil {
		t.Fatal(err)
	}
}

func seedPhase(t *testing.T, store *storage.Storage, key string, phase grab.Phase) {
	t.Helper()
	seed(t, store, testEntry(key), grab.State{Phase: phase})
}

func reply(run childRun, ev grab.Event) { run.reply <- eventMsg(ev) }

func testTorrent() *debridTypes.Torrent {
	return &debridTypes.Torrent{ID: "t1", InfoHash: "a", Name: "a", Debrid: "realdebrid",
		Status: debridTypes.TorrentStatusDownloaded}
}

// submitting brings key to Submitting with its submit child running, which it returns.
func (h *actorHarness) submitting(key string) childRun {
	h.t.Helper()
	seedPhase(h.t, h.store, key, grab.PhaseQueued)
	h.mustAsk(key, grab.Event{Kind: grab.EvRecover})
	run := h.next(grab.ChildSubmit)
	if got := h.phase(key); got != grab.PhaseSubmitting {
		h.t.Fatalf("phase %v, want submitting", got)
	}
	return run
}

func TestSecondDownloadedObservationInActingIsRefused(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		seedPhase(t, store, "a", grab.PhaseActing)
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
		h.next(grab.ChildAction)

		err := h.ask("a", eventMsg(grab.Event{Kind: grab.EvProviderObserved, Downloaded: true, AllLinked: true}))
		if !grab.IsRefused(err) {
			t.Fatalf("err %v, want a refusal", err)
		}
		h.none()
		if got := h.a.refused.Load(); got != 1 {
			t.Fatalf("refused %d, want 1", got)
		}
	})
}

func TestStaleChildCannotRestoreACompletedRecord(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		seedPhase(t, store, "a", grab.PhaseComplete)

		h.send(entryMsg{
			exit: true, child: grab.ChildPoll,
			ev: &grab.Event{Kind: grab.EvProviderObserved, Downloaded: false},
		})
		if got := h.diskPhase(); got != grab.PhaseComplete {
			t.Fatalf("disk phase %v, want complete", got)
		}
		if got := h.phase("a"); got != grab.PhaseComplete {
			t.Fatalf("phase %v, want complete", got)
		}
		h.none()
	})
}

func TestNothingWritesAfterDelete(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		seedPhase(t, store, "a", grab.PhaseProvider)
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
		h.next(grab.ChildPoll)

		h.mustAsk("a", grab.Event{Kind: grab.EvDelete})
		reply(h.next(grab.ChildCleanup), grab.Event{Kind: grab.EvCleanupDone})
		synctest.Wait()
		if got := h.diskPhase(); got != grab.PhaseAbsent {
			t.Fatalf("disk phase %v after cleanup, want absent", got)
		}

		h.send(entryMsg{
			exit: true, child: grab.ChildPoll, torrent: testTorrent(),
			ev: &grab.Event{Kind: grab.EvProviderObserved, Downloaded: true, AllLinked: true},
		})
		h.send(entryMsg{ev: &grab.Event{Kind: grab.EvActionDone}})
		if got := h.diskPhase(); got != grab.PhaseAbsent {
			t.Fatalf("disk phase %v after late results, want absent", got)
		}
		if _, ok := h.grabs.get("a"); ok {
			t.Fatal("a deleted grab is still in the read index")
		}
		h.none()
	})
}

func TestOneChildPerKind(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		seedPhase(t, store, "a", grab.PhaseProvider)
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
		h.next(grab.ChildPoll)

		h.mustAsk("a", grab.Event{Kind: grab.EvPollTimer})
		h.none()
		if got := h.a.refused.Load(); got != 1 {
			t.Fatalf("refused %d, want 1", got)
		}
	})
}

func TestLookupWaitsForTheRunningSubmit(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		submit := h.submitting("a")

		h.mustAsk("a", grab.Event{Kind: grab.EvSubmitDeadline})
		h.none()

		submit.reply <- entryMsg{}
		h.next(grab.ChildLookup)
		h.none()
	})
}

func TestCleanupWaitsForEveryChild(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		submit := h.submitting("a")

		h.mustAsk("a", grab.Event{Kind: grab.EvDelete})
		h.none()

		reply(submit, grab.Event{
			Kind: grab.EvSubmitResult, Outcome: grab.OutcomePlacement, Provider: "realdebrid", TorrentID: "t1",
		})
		cleanup := h.next(grab.ChildCleanup)
		want := []placementRef{{Provider: "realdebrid", TorrentID: "t1"}}
		if !slices.Equal(cleanup.call.releases, want) {
			t.Fatalf("cleanup releases %v, want %v", cleanup.call.releases, want)
		}
	})
}

func TestPersistFailureKeepsTheOldState(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		submit := h.submitting("a")

		reply(submit, grab.Event{Kind: grab.EvSubmitResult, Outcome: grab.OutcomePlacement})
		synctest.Wait()
		if got := h.phase("a"); got != grab.PhaseSubmitting {
			t.Fatalf("phase %v after a failed save, want submitting", got)
		}
		if got := h.diskPhase(); got != grab.PhaseSubmitting {
			t.Fatalf("disk phase %v after a failed save, want submitting", got)
		}
		h.none()
	})
}

func TestLibraryIsPublishedBeforeTheActionStarts(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		seedPhase(t, store, "a", grab.PhaseAwaitingLinks)
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})

		reply(h.next(grab.ChildLinks), grab.Event{Kind: grab.EvLinksReady})
		h.next(grab.ChildAction)
		if got, want := h.logged(), []string{"start:links", "publish", "start:action"}; !slices.Equal(got, want) {
			t.Fatalf("order %v, want %v", got, want)
		}
		if _, err := store.Get("a"); err != nil {
			t.Fatalf("library entry: %v", err)
		}
		if got := h.diskPhase(); got != grab.PhaseActing {
			t.Fatalf("disk phase %v, want acting", got)
		}
	})
}

func TestActionNoneRemovesTheRecord(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		e := testEntry("a")
		e.Action = config.DownloadActionNone
		seed(t, store, e, grab.State{Phase: grab.PhaseActing})
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})

		reply(h.next(grab.ChildAction), grab.Event{Kind: grab.EvActionDone})
		cleanup := h.next(grab.ChildCleanup)
		if cleanup.call.cleanup != (grab.Cleanup{}) {
			t.Fatalf("cleanup %+v, want the zero cleanup", cleanup.call.cleanup)
		}
		reply(cleanup, grab.Event{Kind: grab.EvCleanupDone})
		synctest.Wait()
		if got := h.diskPhase(); got != grab.PhaseAbsent {
			t.Fatalf("disk phase %v, want absent", got)
		}
	})
}

func TestInterruptedActionResumes(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		seedPhase(t, store, "a", grab.PhaseActing)
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})

		h.next(grab.ChildAction).reply <- entryMsg{exitErr: errors.New("mount stopping")}
		synctest.Wait()
		if got := h.diskPhase(); got != grab.PhaseActing {
			t.Fatalf("disk phase %v after an interrupted action, want acting", got)
		}
		h.none()

		time.Sleep(throttledRetryDelay(0))
		h.next(grab.ChildAction)
	})
}

func TestRefusedAdmittedReleasesTheSlot(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		seedPhase(t, store, "a", grab.PhaseProvider)
		h.a.admit.Request("a")
		synctest.Wait()

		h.submitting("b")
	})
}

func TestDocumentLoadErrorUsesTheDerivedState(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		e := testEntry("a")
		seed(t, store, e, grab.State{Phase: grab.PhaseActing})
		e.Grab = []byte("{")
		if err := store.UpdateQueuedGrab(e); err != nil {
			t.Fatal(err)
		}
		loaded, err := store.GetQueuedGrab("a")
		if err != nil {
			t.Fatal(err)
		}
		want := grab.FromLegacy(loaded, nil, false).Phase

		if askErr := h.ask("a", eventMsg(grab.Event{Kind: grab.EvTagsChanged})); askErr != nil {
			t.Fatal(askErr)
		}
		if got := h.phase("a"); got != want {
			t.Fatalf("phase %v, want the derived %v", got, want)
		}
	})
}

func TestStoreReadErrorRefusesCommands(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		seedPhase(t, store, "a", grab.PhaseProvider)
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}

		err := h.ask("a", eventMsg(grab.Event{Kind: grab.EvRecover}))
		if err == nil || grab.IsRefused(err) {
			t.Fatalf("err %v, want the read error", err)
		}
		h.none()
	})
}

func TestRetryAfterDeleteStartsNothing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		phase grab.Phase
		child grab.ChildKind
		retry entryMsg
	}{
		{"poll", grab.PhaseProvider, grab.ChildPoll, eventMsg(grab.Event{Kind: grab.EvPollTimer})},
		{"links", grab.PhaseAwaitingLinks, grab.ChildLinks, entryMsg{retry: true, child: grab.ChildLinks}},
		{"action", grab.PhaseActing, grab.ChildAction, entryMsg{retry: true, child: grab.ChildAction}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			synctest.Test(t, func(t *testing.T) {
				h := newActorHarness(t, store)
				seedPhase(t, store, "a", tc.phase)
				h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
				h.next(tc.child).reply <- entryMsg{}
				synctest.Wait()

				h.mustAsk("a", grab.Event{Kind: grab.EvDelete})
				h.next(grab.ChildCleanup)

				h.send(tc.retry)
				time.Sleep(time.Hour)
				h.none()
			})
		})
	}
}

func TestRecoverInActingRepublishesAMissingLibraryEntry(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		seedPhase(t, store, "a", grab.PhaseActing)
		if _, err := store.Get("a"); err == nil {
			t.Fatal("library entry present before Recover")
		}

		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
		h.next(grab.ChildAction)
		if _, err := store.Get("a"); err != nil {
			t.Fatalf("library entry after Recover: %v", err)
		}
	})
}

func TestSeasonEnteringActingWritesNoLibraryEntry(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		if err := h.ask("a", entryMsg{
			ev: &grab.Event{Kind: grab.EvSubmit, PackKey: "pack"}, entry: testEntry("a"),
		}); err != nil {
			t.Fatal(err)
		}
		h.next(grab.ChildAction)
		if got := h.diskPhase(); got != grab.PhaseActing {
			t.Fatalf("disk phase %v, want acting", got)
		}
		if slices.Contains(h.logged(), "publish") {
			t.Fatal("a season child published a library entry")
		}
		if _, err := store.Get("a"); err == nil {
			t.Fatal("a season child has a library entry")
		}
	})
}

// stop ends the harness's actors and their children, as a manager's stop does.
func (h *actorHarness) stop() {
	h.t.Helper()
	h.down = true
	ctx, cancel := task.Detached(h.t.Context(), time.Minute)
	defer cancel()
	if err := h.root.Stop(ctx); err != nil {
		h.t.Error(err)
	}
}

// manager is a Manager that submits to and recovers through the harness's actor.
func (h *actorHarness) manager() *Manager {
	return &Manager{entries: h.a, grabs: h.grabs, storage: h.store, logger: zerolog.Nop()}
}
