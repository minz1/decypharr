package manager

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/customerror"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
)

type awaitingSelectionProvider struct {
	debrid.Client

	polls, allowed atomic.Int32
}

func (p *awaitingSelectionProvider) Poll(
	_ context.Context,
	t *types.Torrent,
	allowSelect bool,
) (*types.Torrent, types.Observation, error) {
	p.polls.Add(1)
	if allowSelect {
		p.allowed.Add(1)
	}
	return t, types.Observation{Phase: types.PhaseAwaitingSelection, Nudged: allowSelect}, nil
}

func TestPollsAllowThreeSelectsThenFailWithNoEffect(t *testing.T) {
	t.Parallel()
	selectsThenFail(t, 0)
}

func TestSelectsAreCountedAcrossAnIdleActor(t *testing.T) {
	t.Parallel()
	selectsThenFail(t, 2*time.Minute)
}

func selectsThenFail(t *testing.T, gap time.Duration) {
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, time.Hour)
		m, _ := newDroppedTestManager(t, nil)
		m.store = testConfigStore(t, nil)
		provider := &awaitingSelectionProvider{}
		m.clients.Store("realdebrid", provider)
		h.a.child = func(call entryChildCall) func(context.Context) entryMsg {
			return func(ctx context.Context) entryMsg { return m.pollChild(ctx, call, false) }
		}
		e := testEntry("a")
		e.AddedOn = time.Now()
		seed(t, store, e, grab.State{Phase: grab.PhaseProvider})
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
		synctest.Wait()
		for range maxSelects {
			time.Sleep(gap)
			h.mustAsk("a", grab.Event{Kind: grab.EvPollTimer})
			synctest.Wait()
		}
		polls, allowed := provider.polls.Load(), provider.allowed.Load()
		if polls != maxSelects+1 || allowed != maxSelects {
			t.Fatalf("polls %d with %d allowed to select, want %d and %d", polls, allowed, maxSelects+1, maxSelects)
		}
		if got := h.failsSeen(); len(got) != 1 || got[0] != grab.FailNoEffect {
			t.Fatalf("fail effects = %v, want one FailNoEffect", got)
		}
	})
}

func pollChildOutcome(t *testing.T, provider *failingStatusProvider, uncached, allowSelect bool) entryMsg {
	t.Helper()
	m, entry := queuedEntryAddedAt(t, provider, time.Now(), time.Time{})
	entry.DownloadUncached = uncached
	msg := m.pollChild(t.Context(), entryChildCall{kind: grab.ChildPoll, grab: grab.Grab{Entry: entry},
		allowSelect: allowSelect}, false)
	closePlacements(t, m)
	return msg
}

func TestPollChildFailsAnUncachedTorrentUnlessUncachedIsAllowed(t *testing.T) {
	t.Parallel()
	provider := func() *failingStatusProvider {
		return &failingStatusProvider{phase: types.PhaseDownloading, cached: types.CachedNo}
	}
	failed := pollChildOutcome(t, provider(), false, false)
	if failed.ev == nil || failed.ev.Observed != types.PhaseFailed ||
		!strings.Contains(failed.ev.Reason, customerror.ErrTorrentNotCached.Error()) {
		t.Fatalf("event = %+v, want a failure naming the torrent as not cached", failed.ev)
	}
	allowed := pollChildOutcome(t, provider(), true, false)
	if allowed.ev == nil || allowed.ev.Observed != types.PhaseDownloading {
		t.Fatalf("event = %+v, want a downloading observation", allowed.ev)
	}
}

func TestPollChildFailsASelectionTheProviderNeverTook(t *testing.T) {
	t.Parallel()
	provider := func() *failingStatusProvider { return &failingStatusProvider{phase: types.PhaseAwaitingSelection} }
	spent := pollChildOutcome(t, provider(), false, false)
	if spent.ev == nil || spent.ev.Observed != types.PhaseFailed || spent.ev.Fail != grab.FailNoEffect ||
		spent.ev.Reason != "provider never accepted the file selection" {
		t.Fatalf("event = %+v, want FailNoEffect for a selection never taken", spent.ev)
	}
	open := pollChildOutcome(t, provider(), false, true)
	if open.ev == nil || open.ev.Observed != types.PhaseAwaitingSelection {
		t.Fatalf("event = %+v, want an awaiting_selection observation", open.ev)
	}
}
