package manager

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/proptest"
)

type holderOpKind uint8

const (
	holderOpHold holderOpKind = iota
	holderOpReleaseDelete
	holderOpReleaseOwnLibrary
	holderOpRelease
	holderOpStale
	holderOpLibrary
	holderOpFail
	holderOpSleep
)

type holderOp struct {
	kind    holderOpKind
	grab    int
	torrent int
	wait    bool
}

func holderGrabs() []string    { return []string{"a", "b", "c"} }
func holderTorrents() []string { return []string{"t1", "t2"} }

func (o holderOp) String() string {
	g, tor := holderGrabs()[o.grab], holderTorrents()[o.torrent]
	switch o.kind {
	case holderOpHold:
		return fmt.Sprintf("hold(%s,%s)", g, tor)
	case holderOpReleaseDelete:
		return fmt.Sprintf("releaseDelete(%s,%s,wait=%v)", g, tor, o.wait)
	case holderOpReleaseOwnLibrary:
		return fmt.Sprintf("releaseDeleteOwnLibrary(%s,%s,wait=%v)", g, tor, o.wait)
	case holderOpRelease:
		return fmt.Sprintf("release(%s,%s)", g, tor)
	case holderOpStale:
		return fmt.Sprintf("stale(%s,%s)", g, tor)
	case holderOpLibrary:
		return fmt.Sprintf("library(%s,%s)", g, tor)
	case holderOpFail:
		return "failNext"
	case holderOpSleep:
		return "sleep"
	}
	return "?"
}

func genHolderOps(seed int64) []holderOp {
	r := rand.New(rand.NewPCG(uint64(seed), 7))
	ops := make([]holderOp, 40)
	for i := range ops {
		op := holderOp{grab: r.IntN(len(holderGrabs())), torrent: r.IntN(len(holderTorrents())), wait: r.IntN(2) == 0}
		switch n := r.IntN(20); {
		case n < 6:
			op.kind = holderOpHold
		case n < 9:
			op.kind = holderOpReleaseDelete
		case n < 10:
			op.kind = holderOpReleaseOwnLibrary
		case n < 12:
			op.kind = holderOpRelease
		case n < 13:
			op.kind = holderOpStale
		case n < 15:
			op.kind = holderOpLibrary
		case n < 16:
			op.kind = holderOpFail
		default:
			op.kind = holderOpSleep
		}
		ops[i] = op
	}
	return ops
}

func simplifyHolderOp(o holderOp) []holderOp {
	if o.wait {
		o.wait = false
		return []holderOp{o}
	}
	return nil
}

type holderRun struct {
	t          *testing.T
	w          *holderWorld
	a          *holderActor
	expectGone map[string]int
	lastDel    map[string]map[string]bool
	failed     bool
}

func (r *holderRun) apply(op holderOp) {
	w, t := r.w, r.t
	grab, tor := holderGrabs()[op.grab], holderTorrents()[op.torrent]
	r.track(op, grab, tor)
	switch op.kind {
	case holderOpHold:
		if w.isDeleted(tor) {
			r.expectGone[grab+"/"+tor]++
		}
		w.set(w.rec, grab, tor, true)
		sendHold(t, r.a, tor, grab)
	case holderOpReleaseDelete, holderOpReleaseOwnLibrary:
		w.set(w.rec, grab, tor, false)
		ev := release(grab, true, nil)
		ev.library = op.kind == holderOpReleaseOwnLibrary
		if ev.library {
			w.set(w.lib, grab, tor, false)
		}
		if op.wait {
			ev.done = make(chan releaseResult, 1)
		}
		sendRelease(t, r.a, tor, ev)
	case holderOpRelease:
		w.set(w.rec, grab, tor, false)
		sendRelease(t, r.a, tor, release(grab, false, nil))
	case holderOpStale:
		w.set(w.rec, grab, tor, false)
	case holderOpLibrary:
		w.set(w.lib, grab, tor, !w.libListed(grab, tor))
	case holderOpFail:
		w.setFail(1)
	case holderOpSleep:
		time.Sleep(30 * time.Second)
	}
	synctest.Wait()
}

func (r *holderRun) track(op holderOp, grab, tor string) {
	switch op.kind {
	case holderOpFail:
		r.failed = true
		return
	case holderOpLibrary, holderOpSleep:
		return
	case holderOpHold, holderOpReleaseDelete, holderOpReleaseOwnLibrary, holderOpRelease, holderOpStale:
	}
	if r.lastDel[tor] == nil {
		r.lastDel[tor] = map[string]bool{}
	}
	r.lastDel[tor][grab] = op.kind == holderOpReleaseDelete || op.kind == holderOpReleaseOwnLibrary
}

func (r *holderRun) checkNoLeak() {
	w := r.w
	if r.failed {
		return
	}
	for tor, last := range r.lastDel {
		if w.isDeleted(tor) || !slices.Contains(slices.Collect(maps.Values(last)), true) {
			continue
		}
		leaked := !slices.Contains(slices.Collect(maps.Values(last)), false)
		for _, grab := range holderGrabs() {
			leaked = leaked && !w.libListed(grab, tor)
		}
		if leaked {
			w.violatef("%s: every grab released it with delete and nothing lists it, but it was never deleted", tor)
		}
	}
}

func (r *holderRun) finish() {
	w, t := r.w, r.t
	time.Sleep(10 * time.Minute)
	synctest.Wait()
	r.checkNoLeak()
	w.setFail(0)
	for _, tor := range holderTorrents() {
		for _, grab := range holderGrabs() {
			w.set(w.rec, grab, tor, false)
			w.set(w.lib, grab, tor, false)
			sendRelease(t, r.a, tor, release(grab, false, nil))
		}
		synctest.Wait()
		done := make(chan releaseResult, 1)
		ev := release("a", true, done)
		ev.library = true
		sendRelease(t, r.a, tor, ev)
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		if res := <-done; res.outcome != releaseRemoved {
			w.violatef("%s: once every grab released, Release{delete} gave %v", tor, res.outcome)
		}
	}
	for key, n := range r.expectGone {
		if got := w.goneCalls(key); got < n {
			w.violatef("%s held a deleted torrent %d times and was told %d times", key, n, got)
		}
	}
}

func runHolderOps(t *testing.T, ops []holderOp) []string {
	t.Helper()
	var broken []string
	synctest.Test(t, func(t *testing.T) {
		w := newHolderWorld()
		w.check = true
		r := &holderRun{
			t: t, w: w, a: w.newHolder(t), expectGone: map[string]int{}, lastDel: map[string]map[string]bool{},
		}
		for _, op := range ops {
			r.apply(op)
		}
		r.finish()
		broken = slices.Clone(w.violation)
	})
	return broken
}

func TestHolderProperty(t *testing.T) {
	t.Parallel()
	proptest.CheckWhy(t, proptest.Config{Seeds: 30},
		genHolderOps,
		func(ops []holderOp) []string { return runHolderOps(t, ops) },
		simplifyHolderOp,
	)
}
