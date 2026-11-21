package manager

import (
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const unnamedSuffix = "; placement has no provider id, not blocklisting"

func verdictEntry(id string) *storage.Entry {
	e := testEntry("a")
	e.AddedOn = time.Now()
	e.Providers = map[string]*storage.ProviderEntry{
		"realdebrid": {Provider: "realdebrid", ID: id, Status: debridTypes.TorrentStatusDownloading},
	}
	return e
}

type verdictProducer func(t *testing.T, h *actorHarness, e *storage.Entry)

func pollerFailing(answer pollAnswer) verdictProducer {
	return func(t *testing.T, h *actorHarness, e *storage.Entry) {
		h.linksProvider(t, answer)
		seed(t, h.store, e, grab.State{Phase: grab.PhaseProvider})
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
		synctest.Wait()
	}
}

func verdictProducers() map[string]verdictProducer {
	return map[string]verdictProducer{
		"poll failed": pollerFailing(pollAnswer{
			obs: debridTypes.Observation{Phase: debridTypes.PhaseFailed, Raw: "boom"},
		}),
		"placement gone": pollerFailing(pollAnswer{err: customerror.ErrTorrentNotFound}),
		"pending dead": func(t *testing.T, h *actorHarness, e *storage.Entry) {
			seed(t, h.store, e, grab.State{Phase: grab.PhaseProvider})
			h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
			run := h.next(grab.ChildPoll)
			run.reply <- entryMsg{seen: &pollSeen{pending: grab.PendingConverting, dead: true}}
			synctest.Wait()
			time.Sleep(3 * time.Hour)
		},
		"links expired": func(t *testing.T, h *actorHarness, e *storage.Entry) {
			at := time.Now().Add(-7 * time.Hour)
			e.Providers["realdebrid"].DownloadedAt = &at
			seed(t, h.store, e, grab.State{Phase: grab.PhaseAwaitingLinks})
			h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
			run := h.next(grab.ChildLinks)
			time.Sleep(time.Hour)
			run.reply <- entryMsg{linksMissing: &placementRef{Provider: "realdebrid", TorrentID: e.Providers["realdebrid"].ID}}
		},
	}
}

type verdictWant struct {
	fail   grab.FailEffect
	arr    grab.ArrFail
	suffix bool
}

func withheld() verdictWant { return verdictWant{fail: grab.FailEntry, suffix: true} }

func blocked() verdictWant {
	return verdictWant{
		fail: grab.FailBadRelease, arr: grab.ArrFail{State: grab.ArrFailPending, Arr: "sonarr"},
	}
}

func blockedNoArr() verdictWant { return verdictWant{fail: grab.FailBadRelease} }

func runVerdict(t *testing.T, mk func() *storage.Entry, produce verdictProducer, want verdictWant) {
	t.Helper()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		h.a.failArr = func(grab.Grab) string { return "sonarr" }
		produce(t, h, mk())
		h.mustBe("a", grab.PhaseFailed)
		h.mustFailWith(want.fail)
		disk, _ := h.diskGrab("a")
		if disk.State.ArrFail != want.arr {
			t.Fatalf("arr_fail %+v, want %+v", disk.State.ArrFail, want.arr)
		}
		reason := disk.State.Failure.Reason
		if got := strings.HasSuffix(reason, unnamedSuffix); got != want.suffix {
			t.Fatalf("reason %q: suffix = %v, want %v", reason, got, want.suffix)
		}
	})
}

func TestPlacementVerdictWithheldOnUnnamedPlacement(t *testing.T) {
	t.Parallel()
	for name, produce := range verdictProducers() {
		for _, id := range []string{"", "0"} {
			t.Run(name+"/id "+id, func(t *testing.T) {
				t.Parallel()
				runVerdict(t, func() *storage.Entry { return verdictEntry(id) }, produce, withheld())
			})
		}
	}
}

func TestPlacementVerdictKeptOnANamedPlacement(t *testing.T) {
	t.Parallel()
	for name, produce := range verdictProducers() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runVerdict(t, func() *storage.Entry { return verdictEntry("RD1") }, produce, blocked())
		})
	}
}

func TestPlacementVerdictKeptWhenTheActivePlacementIsNamed(t *testing.T) {
	t.Parallel()
	mk := func() *storage.Entry {
		e := verdictEntry("RD1")
		e.Providers["torbox"] = &storage.ProviderEntry{Provider: "torbox", ID: "0"}
		return e
	}
	runVerdict(t, mk, verdictProducers()["poll failed"], blocked())
}

func TestPlacementVerdictKeptForAnNZBGrab(t *testing.T) {
	t.Parallel()
	mk := func() *storage.Entry {
		e := verdictEntry("")
		e.Protocol = config.ProtocolNZB
		return e
	}
	runVerdict(t, mk, verdictProducers()["poll failed"], blockedNoArr())
}

func TestPlacementVerdictKeptForASeasonChild(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, 0)
		h.a.failArr = func(grab.Grab) string { return "sonarr" }
		e := verdictEntry("")
		e.Providers["realdebrid"].Status = debridTypes.TorrentStatusDownloaded
		seed(t, store, e, grab.State{Phase: grab.PhaseComplete, PackKey: "pack"})
		h.mustAsk("a", grab.Event{Kind: grab.EvProviderDropped, Fail: grab.FailBadRelease, Reason: "bad"})
		h.mustBe("a", grab.PhaseFailed)
		h.mustFailWith(grab.FailBadRelease)
		disk, _ := h.diskGrab("a")
		if disk.State.Failure.Reason != "bad" || disk.State.ArrFail != (grab.ArrFail{}) {
			t.Fatalf("reason %q, arr_fail %+v, want bad and none", disk.State.Failure.Reason, disk.State.ArrFail)
		}
	})
}

func TestPlacementVerdictPure(t *testing.T) {
	t.Parallel()
	unnamed := func() *storage.Entry {
		e := verdictEntry("0")
		e.SetActive("realdebrid", true)
		return e
	}
	bad := grab.Event{Kind: grab.EvProviderObserved, Fail: grab.FailBadRelease, Reason: "boom"}
	for name, tc := range map[string]struct {
		g      grab.Grab
		ev     grab.Event
		want   grab.Event
		wantOK bool
	}{
		"unnamed": {
			grab.Grab{Entry: unnamed()}, bad,
			grab.Event{Kind: grab.EvProviderObserved, Fail: grab.FailEntry, Reason: "boom" + unnamedSuffix}, true,
		},
		"empty reason": {
			grab.Grab{Entry: unnamed()}, grab.Event{Fail: grab.FailBadRelease},
			grab.Event{Fail: grab.FailEntry}, true,
		},
		"no record":      {grab.Grab{}, bad, bad, false},
		"no active":      {grab.Grab{Entry: verdictEntry("")}, bad, bad, false},
		"season child":   {grab.Grab{State: grab.State{PackKey: "pack"}, Entry: unnamed()}, bad, bad, false},
		"nzb":            {grab.Grab{State: grab.State{NZB: true}, Entry: unnamed()}, bad, bad, false},
		"no effect fail": {grab.Grab{Entry: unnamed()}, grab.Event{Fail: grab.FailNoEffect, Reason: "x"}, grab.Event{Fail: grab.FailNoEffect, Reason: "x"}, false},
		"submit result": {
			grab.Grab{State: grab.State{Phase: grab.PhaseSubmitting}, Entry: unnamed()},
			grab.Event{Kind: grab.EvSubmitResult, Outcome: grab.OutcomePermanent, Fail: grab.FailBadRelease, Reason: "refused"},
			grab.Event{Kind: grab.EvSubmitResult, Outcome: grab.OutcomePermanent, Fail: grab.FailBadRelease, Reason: "refused"},
			false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := placementVerdict(tc.g, tc.ev)
			if ok != tc.wantOK || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("placementVerdict = %+v, %v; want %+v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestSubmitRefusalOnReaddedUnnamedPlacementStillBlocklists(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := deadlineHarness(t, store, noPoll)
		h.a.failArr = func(grab.Grab) string { return "sonarr" }
		e := verdictEntry("")
		e.SetActive("realdebrid", true)
		seed(t, store, e, failedState())
		h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
		if err := h.manager().submitEntry(t.Context(), testEntry("a"), addReq()); err != nil {
			t.Fatal(err)
		}
		run := h.next(grab.ChildSubmit)
		reply(run, grab.Event{
			Kind: grab.EvSubmitResult, Outcome: grab.OutcomePermanent, Fail: grab.FailBadRelease, Reason: "refused",
		})
		h.mustBe("a", grab.PhaseFailed)
		h.mustFailWith(grab.FailBadRelease)
		disk, _ := h.diskGrab("a")
		if want := (grab.ArrFail{State: grab.ArrFailPending, Arr: "sonarr"}); disk.State.ArrFail != want ||
			disk.State.Failure.Reason != "refused" {
			t.Fatalf("arr_fail %+v, reason %q; want %+v, refused", disk.State.ArrFail, disk.State.Failure.Reason, want)
		}
	})
}
