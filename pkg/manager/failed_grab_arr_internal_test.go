package manager

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestOnlyAFailureAnArrCanSeeAndRemoveIsOwnedByTheArr(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		edit func(*storage.Entry, *grab.State)
		want bool
	}{
		"arr torrent":  {func(e *storage.Entry, _ *grab.State) { e.Protocol = config.ProtocolTorrent }, true},
		"arr nzb":      {func(e *storage.Entry, _ *grab.State) { e.Protocol = config.ProtocolNZB }, true},
		"no arr":       {func(e *storage.Entry, _ *grab.State) { e.Category = "bindery" }, false},
		"manual entry": {func(e *storage.Entry, _ *grab.State) { e.Manual = true }, false},
		"season child": {func(_ *storage.Entry, st *grab.State) { st.PackKey = "pack" }, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m, entry := newDroppedTestManager(t, nil)
			st := grab.State{Phase: grab.PhaseFailed}
			tc.edit(entry, &st)
			if got := m.arrOwnsFailure(grab.Grab{Entry: entry, State: st}); got != tc.want {
				t.Fatalf("arrOwnsFailure = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestAFailedGrabAnArrOwnsIsNeverRemovedOnItsOwn(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, time.Hour)
		h.a.arrOwnsFailure = func(grab.Grab) bool { return true }
		h.failedGrab("a", time.Now())
		h.arrSees("a")
		time.Sleep(10 * time.Hour)
		h.mustBe("a", grab.PhaseFailed)
		h.none()
	})
}

func TestAFailedGrabNoArrOwnsIsRemovedAtItsRetention(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, time.Hour)
		h.a.arrOwnsFailure = func(grab.Grab) bool { return false }
		h.failedGrab("a", time.Now())
		h.arrSees("a")
		time.Sleep(time.Hour + time.Second)
		h.removedAfterCleanup("a")
	})
}

func TestAnArrRegisteredAfterTheRetentionWasArmedKeepsTheFailedGrab(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		var owned atomic.Bool
		h := deadlineHarness(t, store, time.Hour)
		h.a.arrOwnsFailure = func(grab.Grab) bool { return owned.Load() }
		h.failedGrab("a", time.Now())
		h.arrSees("a")
		owned.Store(true)
		time.Sleep(10 * time.Hour)
		h.mustBe("a", grab.PhaseFailed)
		h.none()
	})
}

func TestASeasonChildsBadReleaseNeverCallsTheArr(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{}
	m, entry := newDroppedTestManager(t, fake)
	entry.Protocol = config.ProtocolTorrent
	g := failedGrab(entry, "entry in error state on debrid")
	g.State.PackKey = "pack"
	if got := m.failArr(g); got != "" {
		t.Fatalf("failArr = %q, want none for a season child's synthetic hash", got)
	}
}

func TestAFailedNZBAnArrOwnsOutlivesTheNZBRetention(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, time.Hour)
		h.a.arrOwnsFailure = func(grab.Grab) bool { return true }
		e := testEntry("a")
		e.Protocol = config.ProtocolNZB
		seed(t, store, e, grab.State{
			Phase: grab.PhaseFailed, NZB: true, Failure: grab.Failure{Reason: "x", At: time.Now(), Count: 1},
		})
		h.arrSees("a")
		time.Sleep(grab.NZBRetention + 10*time.Hour)
		h.mustBe("a", grab.PhaseFailed)
		h.none()
	})
}
