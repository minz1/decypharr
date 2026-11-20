package manager

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type scriptedRecovery struct {
	*fakeArrRecovery

	mu        sync.Mutex
	saved     map[string]reacquire.Job
	handed    reacquire.Handed
	onFail    func()
	downloads int
	jobCalls  int
}

func newScriptedRecovery() *scriptedRecovery {
	return &scriptedRecovery{
		fakeArrRecovery: &fakeArrRecovery{},
		saved:           map[string]reacquire.Job{},
		handed:          reacquire.HandedCreated,
	}
}

func (r *scriptedRecovery) FailReceipt(_, downloadID string, _ time.Time) (reacquire.Job, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobCalls++
	job, ok := r.saved[strings.ToLower(downloadID)]
	return job, ok, nil
}

func (r *scriptedRecovery) FailDownload(
	arrName, downloadID, entryID string, since time.Time,
) (reacquire.Job, reacquire.Handed, error) {
	r.mu.Lock()
	r.downloads++
	onFail, handed := r.onFail, r.handed
	job, saved := r.saved[entryID]
	r.mu.Unlock()
	if onFail != nil {
		onFail()
	}
	if saved {
		return job, reacquire.HandedExisting, nil
	}
	job, _, err := r.fakeArrRecovery.FailDownload(arrName, downloadID, entryID, since)
	return job, handed, err
}

func (r *scriptedRecovery) downloadCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.downloads
}

func freeSlots(t *testing.T, m *Manager) int {
	t.Helper()
	return freeSlotsFor(t, m, "realdebrid")
}

func freeSlotsFor(t *testing.T, m *Manager, provider string) int {
	t.Helper()
	var probes []string
	for i := range 10 {
		id := "probe" + string(rune('a'+i))
		if err := m.arr.AdmitFailGrab(t.Context(), "sonarr", id, provider); err != nil {
			break
		}
		probes = append(probes, id)
	}
	for _, id := range probes {
		m.arr.ReleaseFailGrab("sonarr", id, provider)
	}
	return len(probes)
}

func deliveryOf(entry *storage.Entry) failDelivery {
	return failDelivery{
		arr: "sonarr", provider: "realdebrid", downloadID: strings.ToUpper(entry.InfoHash), entryID: entry.InfoHash,
		name:   entry.Name,
		reason: "Provider rejected the torrent", since: time.Now(),
	}
}

func TestDeliverFailGuardBookkeeping(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		handed     reacquire.Handed
		wantErr    bool
		wantSlots  int
		wantCalls  int
		current    func(cancel context.CancelFunc) func() bool
		savedFirst bool
		precheck   bool
	}{
		"created counts against the bucket": {handed: reacquire.HandedCreated, wantSlots: 1, wantCalls: 1},
		"a receipt that appears during admission is released": {
			handed: reacquire.HandedExisting, wantSlots: 2, wantCalls: 1,
		},
		"a re-fail during admission sends nothing and gives the place back": {
			handed: reacquire.HandedCreated, wantErr: true, wantSlots: 2, wantCalls: 0,
			current: func(context.CancelFunc) func() bool { return func() bool { return false } },
		},
		"a stop after admission sends nothing and gives the place back": {
			handed: reacquire.HandedCreated, wantErr: true, wantSlots: 2, wantCalls: 0,
			current: func(cancel context.CancelFunc) func() bool { return func() bool { cancel(); return true } },
		},
		"a receipt found before admission skips the guard and FailDownload": {
			handed: reacquire.HandedExisting, wantSlots: 2, wantCalls: 0, savedFirst: true,
		},
		"a receipt miss then a created job is admitted and committed": {
			handed: reacquire.HandedCreated, wantSlots: 1, wantCalls: 1, precheck: true,
		},
		"a receipt miss then a requeued job is admitted and committed": {
			handed: reacquire.HandedRequeued, wantSlots: 1, wantCalls: 1, precheck: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			recovery := newScriptedRecovery()
			recovery.handed = tc.handed
			var verdict atomic.Int64
			verdict.Store(int64(arr.CanaryFail))
			m, _ := grabManager(t, recovery.fakeArrRecovery, &verdict)
			entry := grabEntry(1)
			if tc.savedFirst {
				recovery.saved[entry.InfoHash] = reacquire.Job{ID: "saved"}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			d := deliveryOf(entry)
			d.precheck = tc.savedFirst || tc.precheck
			if tc.current != nil {
				d.current = tc.current(cancel)
			}
			_, err := m.deliverFail(ctx, recovery, d)
			if (err != nil) != tc.wantErr {
				t.Fatalf("deliverFail error = %v, want error = %v", err, tc.wantErr)
			}
			if got := recovery.downloadCalls(); got != tc.wantCalls {
				t.Fatalf("FailDownload calls = %d, want %d", got, tc.wantCalls)
			}
			if got := freeSlots(t, m); got != tc.wantSlots {
				t.Fatalf("free slots in the bucket = %d, want %d", got, tc.wantSlots)
			}
		})
	}
}

func TestDeliverFailResultsAreClassified(t *testing.T) {
	t.Parallel()
	recovery := newScriptedRecovery()
	var verdict atomic.Int64
	verdict.Store(int64(arr.CanaryFail))
	m, _ := grabManager(t, recovery.fakeArrRecovery, &verdict)
	for i := range 2 {
		if err := m.failGrab(grabEntry(i), recovery, "sonarr", "free"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := m.deliverFail(t.Context(), recovery, deliveryOf(grabEntry(5)))
	res := failResultOf(attemptResult{}, err)
	if res.kind != resHeld || !res.retryAt.After(time.Time{}) {
		t.Fatalf("a held grab = %+v, want resHeld with a retry time", res)
	}
	recovery.fakeArrRecovery.failErr = &reacquire.OtherJobError{Strategy: reacquire.StrategyCommandSearch}
	other, err := m.deliverFail(t.Context(), recovery, failDelivery{
		arr: "sonarr", provider: "other", downloadID: "X", entryID: "x",
	})
	if got := failResultOf(attemptResult{}, err); got.kind != resOtherJob || other.ID != "" {
		t.Fatalf("another job active = %+v, want resOtherJob", got)
	}
	recovery.fakeArrRecovery.failErr = errors.New("arr unreachable")
	_, err = m.deliverFail(t.Context(), recovery, failDelivery{
		arr: "sonarr", provider: "other", downloadID: "Y", entryID: "y",
	})
	if got := failResultOf(attemptResult{}, err); got.kind != resError || got.err == nil {
		t.Fatalf("an unreachable arr = %+v, want resError", got)
	}
}

type failerWorld struct {
	h        *actorHarness
	m        *Manager
	recovery *scriptedRecovery
}

func newFailerWorld(
	t *testing.T, h *actorHarness, arrs []config.Arr, entry *storage.Entry, at time.Time, wrap ...func(*arrFailerDeps),
) *failerWorld {
	t.Helper()
	seed(t, h.store, entry, grab.State{
		Phase:   grab.PhaseFailed,
		Failure: grab.Failure{Reason: "x", At: at, Count: 1},
		ArrFail: grab.ArrFail{State: grab.ArrFailPending, Arr: "sonarr"},
	})
	return attachFailer(t, h, arrs, wrap...)
}

func attachFailer(t *testing.T, h *actorHarness, arrs []config.Arr, wrap ...func(*arrFailerDeps)) *failerWorld {
	t.Helper()
	m := h.manager()
	m.work = testGroup(t)
	m.arr = arr.New(config.NewStore(&config.Config{Arrs: arrs}), nil, zerolog.Nop())
	recovery := newScriptedRecovery()
	m.SetArrRecovery(recovery)
	f := m.testFailer(t, wrap...)
	m.arr.SetAddedHook(f.arrAdded)
	return &failerWorld{h: h, m: m, recovery: recovery}
}

func (w *failerWorld) guard(perBucket int, canary func(context.Context, string) arr.CanaryVerdict) {
	w.m.arr = arr.New(config.NewStore(&config.Config{
		Arrs:         sonarrOnly(),
		QueueCleanup: config.QueueCleanup{BlocklistPerBucket: perBucket, BlocklistWindow: "1h"},
	}), nil, zerolog.Nop())
	w.m.arr.SetQueueGuardHooks(arr.QueueGuardHooks{Canary: canary, Notify: func(string, string) {}})
	w.m.arr.SetAddedHook(w.m.failer.arrAdded)
}

func (w *failerWorld) stop() {
	w.h.t.Helper()
	ctx, cancel := task.Detached(w.h.t.Context(), time.Minute)
	defer cancel()
	if err := w.m.work.Stop(ctx); err != nil {
		w.h.t.Fatal(err)
	}
}

func (w *failerWorld) boot() {
	w.h.t.Helper()
	records, err := w.h.store.FilterQueuedGrab(nil)
	if err != nil {
		w.h.t.Fatal(err)
	}
	w.m.recoverEntries(records, nil)
	synctest.Wait()
}

func sonarrOnly() []config.Arr {
	return []config.Arr{{Name: "sonarr", Host: "http://localhost:8989", Token: "x"}}
}

func (w *failerWorld) arrFail() grab.ArrFail {
	w.h.t.Helper()
	g, ok := w.h.diskGrab("a")
	if !ok {
		w.h.t.Fatal("no record for a")
	}
	return g.State.ArrFail
}

func TestArrFailerDeliversAPersistedFailAndRecordsTheJob(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, noPoll)
		w := newFailerWorld(t, h, sonarrOnly(), testEntry("a"), time.Now())
		w.boot()
		if got := failsOf(w.recovery.fakeArrRecovery); len(got) != 1 || got[0] != [3]string{"sonarr", "A", "a"} {
			t.Fatalf("FailDownload calls = %v, want one for the grab", got)
		}
		if got := w.arrFail(); got.State != grab.ArrFailQueued || got.JobID != "fail-1" {
			t.Fatalf("arr_fail on disk = %+v, want queued with the job", got)
		}
	})
}

func TestArrFailerReplaysASavedJobWithoutAnotherFail(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, noPoll)
		w := newFailerWorld(t, h, sonarrOnly(), testEntry("a"), time.Now())
		w.recovery.saved["a"] = reacquire.Job{ID: "saved-1"}
		before := freeSlots(t, w.m)
		w.boot()
		if got := w.arrFail(); got.State != grab.ArrFailQueued || got.JobID != "saved-1" {
			t.Fatalf("arr_fail on disk = %+v, want queued with the saved job", got)
		}
		if got := failsOf(w.recovery.fakeArrRecovery); len(got) != 0 {
			t.Fatalf("a new fail was made: %v", got)
		}
		if got := freeSlots(t, w.m); got != before {
			t.Fatalf("free slots = %d, was %d; want the guard untouched", got, before)
		}
	})
}

func TestArrFailerUsesTheFrozenArr(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, noPoll)
		entry := testEntry("a")
		entry.Category = "renamed"
		w := newFailerWorld(t, h, sonarrOnly(), entry, time.Now())
		w.boot()
		if got := failsOf(w.recovery.fakeArrRecovery); len(got) != 1 || got[0][0] != "sonarr" {
			t.Fatalf("FailDownload calls = %v, want the arr frozen on the grab", got)
		}
	})
}

func TestArrFailerWaitsForTheArrRegistration(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, noPoll)
		w := newFailerWorld(t, h, nil, testEntry("a"), time.Now())
		w.boot()
		time.Sleep(6 * time.Hour)
		synctest.Wait()
		if w.recovery.downloadCalls() != 0 || w.arrFail().State != grab.ArrFailPending {
			t.Fatalf("calls = %d, arr_fail = %+v; want nothing sent before the arr exists",
				w.recovery.downloadCalls(), w.arrFail())
		}
		w.m.arr.AddOrUpdate(arr.Arr{
			Name: "sonarr", Host: "http://localhost:8989", Token: "x", Source: arr.SourceAuto,
		})
		synctest.Wait()
		if w.recovery.downloadCalls() != 1 || w.arrFail().State != grab.ArrFailQueued {
			t.Fatalf("calls = %d, arr_fail = %+v; want delivery on registration",
				w.recovery.downloadCalls(), w.arrFail())
		}
	})
}

func TestTheManagerWiresTheFailerToTheArrService(t *testing.T) {
	t.Parallel()
	m, _ := newLifecycleManager(t)
	if m.failer == nil {
		t.Fatal("the manager built no failer")
	}
	m.arr.AddOrUpdate(arr.Arr{Name: "sonarr", Host: "http://sonarr.example", Token: "t", Source: arr.SourceAuto})
	m.failer.mu.Lock()
	gen := m.failer.arrGen["sonarr"]
	m.failer.mu.Unlock()
	if gen != 1 {
		t.Fatalf("arr generation = %d, want the registration to reach the failer", gen)
	}
}

func firstCallBlocks(entered chan<- struct{}, release <-chan struct{}) func(context.Context, string) arr.CanaryVerdict {
	var calls atomic.Int64
	return func(ctx context.Context, _ string) arr.CanaryVerdict {
		if calls.Add(1) > 1 {
			return arr.CanaryFail
		}
		close(entered)
		select {
		case <-release:
			return arr.CanaryPass
		case <-ctx.Done():
			return arr.CanaryAborted
		}
	}
}

func (w *failerWorld) fillBucket(provider string) {
	w.h.t.Helper()
	if err := w.m.arr.AdmitFailGrab(w.h.t.Context(), "sonarr", "burn", provider); err != nil {
		w.h.t.Fatal(err)
	}
	w.m.arr.CommitFailGrab("sonarr", "burn", provider)
}

func TestArrFailerReaddDuringAdmit(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, noPoll)
		w := newFailerWorld(t, h, sonarrOnly(), testEntry("a"), time.Now())
		entered, release := make(chan struct{}), make(chan struct{})
		w.guard(1, firstCallBlocks(entered, release))
		w.fillBucket("torrent")
		w.boot()
		<-entered
		h.grabs.remove("a")
		close(release)
		synctest.Wait()
		if got := w.recovery.downloadCalls(); got != 0 {
			t.Fatalf("FailDownload calls = %d, want none for an occurrence that was replaced during admission", got)
		}
		if w.m.failer.itemCount() != 0 {
			t.Fatal("the stale item stayed in the failer")
		}
	})
}

func TestArrFailerStopBeforeSave(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, noPoll)
		first := newFailerWorld(t, h, sonarrOnly(), testEntry("a"), time.Now())
		entered, release := make(chan struct{}), make(chan struct{})
		defer func() { close(release); synctest.Wait() }()
		first.guard(1, firstCallBlocks(entered, release))
		first.fillBucket("torrent")
		first.boot()
		<-entered
		first.stop()
		if got := first.recovery.downloadCalls(); got != 0 {
			t.Fatalf("FailDownload calls before the stop = %d, want none", got)
		}
		if got := first.arrFail(); got.State != grab.ArrFailPending {
			t.Fatalf("arr_fail after the stop = %+v, want still pending", got)
		}
		second := attachFailer(t, h, sonarrOnly())
		second.boot()
		if got := failsOf(second.recovery.fakeArrRecovery); len(got) != 1 {
			t.Fatalf("FailDownload calls after the next boot = %v, want exactly one", got)
		}
		if got := second.arrFail(); got.State != grab.ArrFailQueued || got.JobID != "fail-1" {
			t.Fatalf("arr_fail after the next boot = %+v, want queued with the job", got)
		}
	})
}

func TestArrFailerStopAfterSave(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, noPoll)
		cancels := make(chan context.CancelFunc, 1)
		cancelAttempts := func(d *arrFailerDeps) {
			orig := d.startAttempt
			d.startAttempt = func(name string, fn func(ctx context.Context)) error {
				return orig(name, func(ctx context.Context) {
					c, cancel := context.WithCancel(ctx)
					defer cancel()
					cancels <- cancel
					fn(c)
				})
			}
		}
		first := newFailerWorld(t, h, sonarrOnly(), testEntry("a"), time.Now(), cancelAttempts)
		first.recovery.onFail = func() {
			first.recovery.mu.Lock()
			first.recovery.saved["a"] = reacquire.Job{ID: "fail-1"}
			first.recovery.mu.Unlock()
			(<-cancels)()
		}
		first.boot()
		first.stop()
		if got := failsOf(first.recovery.fakeArrRecovery); len(got) != 1 {
			t.Fatalf("FailDownload calls in the first run = %v, want the one that saved the job", got)
		}
		second := attachFailer(t, h, sonarrOnly())
		second.recovery.saved["a"] = reacquire.Job{ID: "fail-1"}
		second.boot()
		if got := failsOf(second.recovery.fakeArrRecovery); len(got) != 0 {
			t.Fatalf("a second job was made after the next boot: %v", got)
		}
		if got := second.arrFail(); got.State != grab.ArrFailQueued || got.JobID != "fail-1" {
			t.Fatalf("arr_fail after the next boot = %+v, want queued with the saved job", got)
		}
	})
}

func TestArrFailerWaitingHistoryJobRequeued(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, noPoll)
		w := newFailerWorld(t, h, sonarrOnly(), testEntry("a"), time.Now())
		w.guard(2, func(context.Context, string) arr.CanaryVerdict { return arr.CanaryFail })
		w.recovery.handed = reacquire.HandedRequeued
		w.boot()
		if w.recovery.jobCalls != 1 || w.recovery.downloadCalls() != 1 {
			t.Fatalf("FailReceipt calls = %d, FailDownload calls = %d, want the waiting job not read as a receipt",
				w.recovery.jobCalls, w.recovery.downloadCalls())
		}
		if got := w.arrFail(); got.State != grab.ArrFailQueued || got.JobID != "fail-1" {
			t.Fatalf("arr_fail = %+v, want queued with the requeued job", got)
		}
		if got := freeSlotsFor(t, w.m, "torrent"); got != 1 {
			t.Fatalf("free slots = %d, want the requeue counted against the bucket", got)
		}
	})
}

func TestArrFailerNilRecovery(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, noPoll)
		at := time.Now()
		w := newFailerWorld(t, h, sonarrOnly(), testEntry("a"), at)
		w.m.SetArrRecovery(nil)
		w.boot()
		st, ok := w.m.failer.status("a", at)
		if !ok || st.LastError == "" || !st.NextRetry.Equal(at.Add(failBackoffBase)) {
			t.Fatalf("status = %+v, %v; want the error and a retry in one backoff step", st, ok)
		}
		if got := w.arrFail(); got.State != grab.ArrFailPending {
			t.Fatalf("arr_fail = %+v, want still pending", got)
		}
		w.m.SetArrRecovery(w.recovery)
		time.Sleep(failBackoffBase)
		synctest.Wait()
		if got := w.arrFail(); got.State != grab.ArrFailQueued {
			t.Fatalf("arr_fail = %+v, want queued once the recovery service runs", got)
		}
	})
}

func TestArrFailerBootWindowNotMarked(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		m := h.manager()
		m.work = testGroup(t)
		m.arr = arr.New(config.NewStore(&config.Config{}), nil, zerolog.Nop())
		recovery := newScriptedRecovery()
		m.SetArrRecovery(recovery)
		f := m.testFailer(t)
		m.arr.SetAddedHook(f.arrAdded)
		h.a.failArr = m.failArr
		h.a.tell = m.tell
		entry := testEntry("a")
		entry.Protocol = config.ProtocolTorrent
		entry.AddedOn = time.Now()
		seed(t, store, entry, grab.State{Phase: grab.PhaseProvider})
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
		run := h.next(grab.ChildPoll)
		run.reply <- entryMsg{seen: &pollSeen{pending: grab.PendingProvider, dead: true}}
		time.Sleep(6 * time.Hour)
		h.mustBe("a", grab.PhaseFailed)
		if g, _ := h.diskGrab("a"); g.State.ArrFail != (grab.ArrFail{}) {
			t.Fatalf("arr_fail = %+v, want none for an Arr that was not registered at the fail", g.State.ArrFail)
		}
		m.arr.AddOrUpdate(arr.Arr{Name: "sonarr", Host: "http://localhost:8989", Token: "x", Source: arr.SourceAuto})
		synctest.Wait()
		if g, _ := h.diskGrab("a"); g.State.ArrFail != (grab.ArrFail{}) || recovery.downloadCalls() != 0 {
			t.Fatalf("arr_fail = %+v, FailDownload calls = %d; want a later registration to leave the grab unmarked",
				g.State.ArrFail, recovery.downloadCalls())
		}
	})
}

func TestAFinishedArrJobReachesTheFailerThroughTheService(t *testing.T) {
	t.Parallel()
	m, _ := newLifecycleManager(t)
	loops := make(chan struct{}, 16)
	m.failer.deps.onLoop = func() {
		select {
		case loops <- struct{}{}:
		default:
		}
	}
	if err := testGroup(t).Go("arr-failer", m.failer.run); err != nil {
		t.Fatal(err)
	}
	if err := m.arrService.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.arrService.FailDownload("sonarr", "ABC", "abc", time.Time{}); err != nil {
		t.Fatal(err)
	}
	for {
		m.failer.mu.Lock()
		gen := m.failer.keyGen[keyGenName("sonarr", "ABC")]
		m.failer.mu.Unlock()
		if gen > 0 {
			return
		}
		select {
		case <-loops:
		case <-time.After(lifecycleWait):
			t.Fatal("a finished job never reached the failer's key generation")
		}
	}
}
