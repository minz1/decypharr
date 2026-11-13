package manager

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type parkKey struct {
	kind grab.ChildKind
	key  string
}

type packTest struct {
	t      *testing.T
	h      *actorHarness
	parked map[parkKey]childRun
	keys   []string

	sharedFolder bool
}

func newPackTest(t *testing.T, store *storage.Storage) *packTest {
	t.Helper()
	h := newActorHarness(t, store)
	p := &packTest{t: t, h: h, parked: map[parkKey]childRun{}}
	for _, n := range []int{1, 2} {
		p.keys = append(p.keys, strings.ToLower(grab.SeasonHash("pack", n)))
	}
	h.a.seasonsOf = func(e *storage.Entry) []*storage.Entry {
		if e.InfoHash != "pack" {
			return nil
		}
		return []*storage.Entry{seasonEntry(p.keys[0]), seasonEntry(p.keys[1])}
	}
	h.a.completed = func(g grab.Grab) { h.record("completed:" + g.State.Key) }
	return p
}

func (p *packTest) drain() {
	p.t.Helper()
	synctest.Wait()
	for {
		select {
		case run := <-p.h.started:
			p.parked[parkKey{run.call.kind, run.call.grab.State.Key}] = run
		default:
			return
		}
	}
}

func (p *packTest) take(kind grab.ChildKind, key string) childRun {
	p.t.Helper()
	p.drain()
	run, ok := p.parked[parkKey{kind, key}]
	if !ok {
		p.t.Fatalf("no %s child running for %s", childKind(kind), key)
	}
	delete(p.parked, parkKey{kind, key})
	return run
}

func seasonEntry(key string) *storage.Entry {
	e := testEntry(key)
	e.Name = key
	return e
}

func (p *packTest) submitSeason(key string) {
	p.t.Helper()
	season := seasonEntry(key)
	if p.sharedFolder && key == p.keys[0] {
		season.Name = "pack"
	}
	msg := entryMsg{ev: &grab.Event{Kind: grab.EvSubmit, PackKey: "pack"}, entry: season}
	if err := p.h.ask(key, msg); err != nil {
		p.t.Fatal(err)
	}
}

func (p *packTest) startPack() {
	p.t.Helper()
	seed(p.t, p.h.store, testEntry("pack"), grab.State{Phase: grab.PhaseActing})
	p.h.mustAsk("pack", grab.Event{Kind: grab.EvRecover})
	reply(p.take(grab.ChildAction, "pack"), grab.Event{Kind: grab.EvSeasonsStarted, Seasons: []int{1, 2}})
	synctest.Wait()
	for _, key := range p.keys {
		p.submitSeason(key)
	}
	p.drain()
}

func (p *packTest) finish(key string) {
	p.t.Helper()
	reply(p.take(grab.ChildAction, key), grab.Event{Kind: grab.EvActionDone})
	synctest.Wait()
}

func (p *packTest) want(key string, phase grab.Phase) {
	p.t.Helper()
	synctest.Wait()
	if got := p.h.phase(key); got != phase {
		p.t.Fatalf("%s is %v, want %v", key, got, phase)
	}
}

func (p *packTest) completions() int {
	n := 0
	for _, s := range p.h.logged() {
		if s == "completed:pack" {
			n++
		}
	}
	return n
}

func TestPackCompletesOnlyAfterEverySeason(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		p := newPackTest(t, store)
		p.startPack()
		p.want("pack", grab.PhaseActing)

		p.finish(p.keys[0])
		p.want(p.keys[0], grab.PhaseComplete)
		p.want("pack", grab.PhaseActing)
		if p.completions() != 0 {
			t.Fatal("the pack completed with a season still running")
		}

		p.finish(p.keys[1])
		p.want("pack", grab.PhaseComplete)
		if got := p.completions(); got != 1 {
			t.Fatalf("completion hook ran %d times, want once", got)
		}
	})
}

func TestPackCompletionNeedsEverySeasonTerminalNotJustOneDone(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		p := newPackTest(t, store)
		p.startPack()

		p.finish(p.keys[1])
		p.want("pack", grab.PhaseActing)
		p.finish(p.keys[0])
		p.want("pack", grab.PhaseComplete)
	})
}

func TestAFailedSeasonDoesNotFailItsPack(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		p := newPackTest(t, store)
		p.startPack()

		reply(p.take(grab.ChildAction, p.keys[0]), grab.Event{Kind: grab.EvActionFailed})
		p.want(p.keys[0], grab.PhaseFailed)
		p.want("pack", grab.PhaseActing)

		p.finish(p.keys[1])
		p.want("pack", grab.PhaseComplete)
		p.want(p.keys[0], grab.PhaseFailed)
		if got := p.completions(); got != 1 {
			t.Fatalf("completion hook ran %d times, want once", got)
		}
	})
}

func TestPackIdleBetweenSeasonsStillCompletes(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		p := newPackTest(t, store)
		p.h.a.retryDelay = func(int) time.Duration { return 2 * time.Minute }
		p.startPack()
		time.Sleep(90 * time.Second)
		synctest.Wait()
		for _, k := range p.h.a.runner.Snapshot().Keys {
			if k.Key == "pack" && k.Running {
				t.Fatal("the pack's goroutine is still up after a minute idle")
			}
		}
		p.want("pack", grab.PhaseActing)

		p.finish(p.keys[0])
		p.finish(p.keys[1])
		p.want("pack", grab.PhaseComplete)
		if got := p.completions(); got != 1 {
			t.Fatalf("completion hook ran %d times, want once", got)
		}
	})
}

func TestPackRestartBetweenSeasonsStillCompletes(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		p := newPackTest(t, store)
		p.startPack()
		p.finish(p.keys[0])
		p.h.stop()

		q := newPackTest(t, store)
		q.h.mustAsk(q.keys[1], grab.Event{Kind: grab.EvRecover})
		q.h.mustAsk("pack", grab.Event{Kind: grab.EvRecover})
		reply(q.take(grab.ChildAction, "pack"), grab.Event{Kind: grab.EvSeasonsStarted, Seasons: []int{1, 2}})
		synctest.Wait()
		q.want("pack", grab.PhaseActing)

		q.finish(q.keys[1])
		q.want("pack", grab.PhaseComplete)
		if got := q.completions(); got != 1 {
			t.Fatalf("completion hook ran %d times after the restart, want once", got)
		}
	})
}

func TestRefusedChildDoneIsRecoveredByTheBackstop(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		p := newPackTest(t, store)
		var refuse atomic.Bool
		refuse.Store(true)
		p.h.a.send = func(key string, m entryMsg) error {
			if refuse.Load() {
				return errors.New("mailbox full")
			}
			return p.h.a.runner.Send(key, m)
		}
		p.startPack()

		p.finish(p.keys[0])
		p.finish(p.keys[1])
		p.want(p.keys[1], grab.PhaseComplete)
		p.want("pack", grab.PhaseActing)

		time.Sleep(throttledRetryDelay(0))
		p.want("pack", grab.PhaseComplete)
		if got := p.completions(); got != 1 {
			t.Fatalf("completion hook ran %d times, want once", got)
		}
	})
}

func TestRefusedSeasonSendRetriesThePack(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		p := newPackTest(t, store)
		m := &Manager{
			entries: p.h.a, storage: store, logger: zerolog.Nop(),
			queue: newQueue(store, nil, zerolog.Nop()),
		}
		pack := testEntry("pack")
		var seasons []SeasonInfo
		for i, key := range p.keys {
			seasons = append(seasons, SeasonInfo{SeasonNumber: i + 1, InfoHash: key, Name: key})
		}
		plain := p.h.a.child
		p.h.a.child = func(call entryChildCall) func(ctx context.Context) entryMsg {
			if call.kind != grab.ChildAction || call.grab.State.Key != "pack" {
				return plain(call)
			}
			work := plain(call)
			return func(ctx context.Context) entryMsg {
				if msg := work(ctx); msg.ev != nil {
					return m.packChild(ctx, call, seasons)
				}
				return entryMsg{}
			}
		}
		var refuse atomic.Bool
		refuse.Store(true)
		p.h.a.send = func(key string, msg entryMsg) error {
			if refuse.Load() {
				return errors.New("mailbox full")
			}
			return p.h.a.runner.Send(key, msg)
		}
		seed(t, store, pack, grab.State{Phase: grab.PhaseActing})
		p.h.mustAsk("pack", grab.Event{Kind: grab.EvRecover})

		reply(p.take(grab.ChildAction, "pack"), grab.Event{Kind: grab.EvActionDone})
		p.drain()
		p.want("pack", grab.PhaseActing)
		for _, key := range p.keys {
			if _, running := p.parked[parkKey{grab.ChildAction, key}]; running {
				t.Fatalf("season %s started although its submit was refused", key)
			}
		}

		refuse.Store(false)
		time.Sleep(throttledRetryDelay(0))
		reply(p.take(grab.ChildAction, "pack"), grab.Event{Kind: grab.EvActionDone})
		for _, key := range p.keys {
			p.take(grab.ChildAction, key)
		}
		p.want("pack", grab.PhaseActing)
	})
}

func TestPackDeleteDeletesLiveSeasonsAndKeepsASharedFolder(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		p := newPackTest(t, store)
		p.sharedFolder = true
		p.startPack()
		shared, own := p.keys[0], p.keys[1]

		p.h.mustAsk("pack", grab.Event{Kind: grab.EvDelete, Cleanup: grab.Cleanup{DeleteFiles: true}})

		sharedCleanup := p.take(grab.ChildCleanup, shared)
		ownCleanup := p.take(grab.ChildCleanup, own)
		if sharedCleanup.call.cleanup.DeleteFiles {
			t.Error("a season sharing the pack's folder was deleted with its files")
		}
		if !ownCleanup.call.cleanup.DeleteFiles {
			t.Error("a season with a folder of its own was deleted without its files")
		}
	})
}

func TestPackIndexLearnsSeasonsOfPacksAddedAfterBoot(t *testing.T) {
	t.Parallel()
	idx := newPackIndex(map[string]string{})
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		p := newPackTest(t, store)
		p.h.a.packAdd = idx.add
		p.submitSeason(p.keys[0])
		if got := idx.snapshot()[p.keys[0]]; got != "pack" {
			t.Fatalf("pack of the season = %q, want pack", got)
		}
	})
	idx.add("PACK", "SEASON")
	if got := idx.snapshot()["season"]; got != "pack" {
		t.Fatalf("keys are not lower-cased: %q", got)
	}
}

func TestMountStopMidSeasonKeepsThePackWaitingAndRerunsTheSeason(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		p := newPackTest(t, store)
		p.startPack()
		cut, done := p.keys[0], p.keys[1]

		p.take(grab.ChildAction, cut).reply <- actionResult(t.Context(), seasonEntry(cut), ErrMountStopping)
		p.finish(done)
		p.want(cut, grab.PhaseActing)
		p.want("pack", grab.PhaseActing)
		if p.completions() != 0 {
			t.Fatal("the pack completed with a season cut short by a mount stop")
		}

		time.Sleep(throttledRetryDelay(0))
		p.finish(cut)
		p.want("pack", grab.PhaseComplete)
	})
}

func TestPackHandsAGoneSeasonOnAgainInsteadOfWaitingForever(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		p := newPackTest(t, store)
		p.startPack()
		gone, kept := p.keys[0], p.keys[1]

		p.h.mustAsk(gone, grab.Event{Kind: grab.EvDelete})
		p.take(grab.ChildAction, gone)
		reply(p.take(grab.ChildCleanup, gone), grab.Event{Kind: grab.EvCleanupDone})
		p.want(gone, grab.PhaseAbsent)
		p.finish(kept)
		p.want("pack", grab.PhaseActing)

		time.Sleep(throttledRetryDelay(0))
		p.want(gone, grab.PhaseActing)
		p.finish(gone)
		p.want("pack", grab.PhaseComplete)
	})
}
