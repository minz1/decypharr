package manager

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type attemptLog struct {
	mu       sync.Mutex
	grabs    []attemptSpec
	acks     []attemptSpec
	rechecks []attemptSpec
	times    []time.Time
	grab     func(ctx context.Context, spec attemptSpec) attemptResult
	ack      func(ctx context.Context, spec attemptSpec) attemptResult
	recheck  func(ctx context.Context, spec attemptSpec) attemptResult
}

func (a *attemptLog) deps() arrFailerDeps {
	return arrFailerDeps{
		attemptGrab: func(ctx context.Context, spec attemptSpec) attemptResult {
			a.mu.Lock()
			a.grabs = append(a.grabs, spec)
			a.times = append(a.times, time.Now())
			fn := a.grab
			a.mu.Unlock()
			if fn == nil {
				return attemptResult{kind: resAcked}
			}
			return fn(ctx, spec)
		},
		attemptAck: func(ctx context.Context, spec attemptSpec) attemptResult {
			a.mu.Lock()
			a.acks = append(a.acks, spec)
			fn := a.ack
			a.mu.Unlock()
			if fn == nil {
				return attemptResult{kind: resAcked, jobID: spec.jobID}
			}
			return fn(ctx, spec)
		},
		attemptRecheck: func(ctx context.Context, spec attemptSpec) attemptResult {
			a.mu.Lock()
			a.rechecks = append(a.rechecks, spec)
			fn := a.recheck
			a.mu.Unlock()
			if fn == nil {
				return attemptResult{kind: resAcked}
			}
			return fn(ctx, spec)
		},
	}
}

func (a *attemptLog) grabKeys() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	keys := make([]string, 0, len(a.grabs))
	for _, spec := range a.grabs {
		keys = append(keys, spec.key)
	}
	return keys
}

func (a *attemptLog) grabCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.grabs)
}

func (a *attemptLog) ackCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.acks)
}

func runFailer(t testing.TB, deps arrFailerDeps) *arrFailer {
	t.Helper()
	g := testGroup(t)
	deps.now = time.Now
	deps.logger = zerolog.Nop()
	if deps.startAttempt == nil {
		deps.startAttempt = func(name string, fn func(ctx context.Context)) error {
			return g.Go(name, func(ctx context.Context) error {
				fn(ctx)
				return nil
			})
		}
	}
	f := newArrFailer(deps)
	if err := g.Go("arr-failer", f.run); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *arrFailer) item(key string) (failItem, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it := f.items[key]
	if it == nil {
		return failItem{}, false
	}
	return *it, true
}

func (f *arrFailer) itemCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.items)
}

func (f *arrFailer) heldUntil(bucket string) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b := f.buckets[bucket]; b != nil {
		return b.heldUntil
	}
	return time.Time{}
}

func pendingGrab(key, provider string, at time.Time) grab.Grab {
	return grab.Grab{
		State: grab.State{
			Key: key, Phase: grab.PhaseFailed,
			Failure: grab.Failure{Reason: "x", At: at, Count: 1},
			ArrFail: grab.ArrFail{State: grab.ArrFailPending, Arr: "sonarr"},
		},
		Entry: &storage.Entry{InfoHash: key, ActiveProvider: provider},
	}
}

func TestArrFailerIgnoresAGrabThatIsNotPending(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		log := &attemptLog{}
		f := runFailer(t, log.deps())
		for _, state := range []grab.ArrFailState{grab.ArrFailNone, grab.ArrFailQueued, grab.ArrFailSkipped} {
			g := pendingGrab("a", "rd", time.Now())
			g.State.ArrFail.State = state
			f.wantGrab(g)
		}
		synctest.Wait()
		if log.grabCount() != 0 || f.itemCount() != 0 {
			t.Fatalf("attempts = %d, items = %d; want none", log.grabCount(), f.itemCount())
		}
	})
}

func TestArrFailerBootTellsOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		log := &attemptLog{grab: func(_ context.Context, _ attemptSpec) attemptResult {
			<-release
			return attemptResult{kind: resAcked}
		}}
		f := runFailer(t, log.deps())
		g := pendingGrab("a", "rd", time.Now())
		f.wantGrab(g)
		f.wantGrab(g)
		synctest.Wait()
		f.wantGrab(g)
		close(release)
		synctest.Wait()
		if log.grabCount() != 1 || f.itemCount() != 0 {
			t.Fatalf("attempts = %d, items = %d; want one attempt and the item gone", log.grabCount(), f.itemCount())
		}
	})
}

func TestArrFailerHeldBucket(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		release := start.Add(10 * time.Minute)
		held := true
		log := &attemptLog{}
		log.grab = func(_ context.Context, spec attemptSpec) attemptResult {
			log.mu.Lock()
			defer log.mu.Unlock()
			if held && spec.key == "a" {
				return attemptResult{kind: resHeld, retryAt: release}
			}
			return attemptResult{kind: resAcked}
		}
		f := runFailer(t, log.deps())
		f.wantGrab(pendingGrab("a", "rd", start))
		f.wantGrab(pendingGrab("b", "rd", start.Add(time.Second)))
		f.wantGrab(pendingGrab("c", "other", start.Add(2*time.Second)))
		synctest.Wait()
		if got := log.sortedGrabKeys(); !slices.Equal(got, []string{"a", "c"}) {
			t.Fatalf("attempts = %v, want the held bucket's head and the other bucket only", got)
		}
		time.Sleep(9 * time.Minute)
		synctest.Wait()
		if got := log.sortedGrabKeys(); !slices.Equal(got, []string{"a", "c"}) {
			t.Fatalf("attempts before the hold ends = %v", got)
		}
		log.mu.Lock()
		held = false
		log.mu.Unlock()
		time.Sleep(time.Minute)
		synctest.Wait()
		if got := log.grabKeys(); len(got) != 4 || !slices.Equal(got[2:], []string{"a", "b"}) {
			t.Fatalf("attempts = %v, want a then b once the hold ends, oldest first", got)
		}
		if f.itemCount() != 0 {
			t.Fatalf("items = %d, want none", f.itemCount())
		}
	})
}

func TestArrFailerBusyBucketNoWake(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		log := &attemptLog{grab: func(_ context.Context, spec attemptSpec) attemptResult {
			if spec.key == "a" {
				<-release
			}
			return attemptResult{kind: resAcked}
		}}
		var loops atomic.Int64
		deps := log.deps()
		deps.onLoop = func() { loops.Add(1) }
		f := runFailer(t, deps)
		f.wantGrab(pendingGrab("a", "rd", time.Now()))
		f.wantGrab(pendingGrab("b", "rd", time.Now().Add(time.Second)))
		synctest.Wait()
		before := loops.Load()
		time.Sleep(24 * time.Hour)
		synctest.Wait()
		if loops.Load() != before || !slices.Equal(log.grabKeys(), []string{"a"}) {
			t.Fatalf("loops = %d (was %d), attempts = %v; want the idle failer to sleep through a day",
				loops.Load(), before, log.grabKeys())
		}
		close(release)
		synctest.Wait()
		if !slices.Equal(log.grabKeys(), []string{"a", "b"}) {
			t.Fatalf("attempts = %v, want b after a finished", log.grabKeys())
		}
	})
}

func TestArrFailerBackoff(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		log := &attemptLog{grab: func(_ context.Context, _ attemptSpec) attemptResult {
			return attemptResult{kind: resError, err: errors.New("arr down")}
		}}
		f := runFailer(t, log.deps())
		f.wantGrab(pendingGrab("a", "rd", start))
		time.Sleep(8*time.Hour + time.Minute)
		synctest.Wait()
		log.mu.Lock()
		var got []time.Duration
		for _, at := range log.times {
			got = append(got, at.Sub(start))
		}
		log.mu.Unlock()
		want := []time.Duration{
			0, time.Minute, 3 * time.Minute, 7 * time.Minute, 15 * time.Minute, 31 * time.Minute, 63 * time.Minute,
			123 * time.Minute, 183 * time.Minute, 243 * time.Minute, 303 * time.Minute, 363 * time.Minute,
			423 * time.Minute,
		}
		if !slices.Equal(got, want) {
			t.Fatalf("attempts at %v, want %v", got, want)
		}
		st, ok := f.status("a", start)
		if !ok || st.LastError != "arr down" || !st.NextRetry.After(time.Now()) {
			t.Fatalf("status = %+v, %v; want the error and a retry in the future", st, ok)
		}
	})
}

func TestArrFailerBurst500(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		log := &attemptLog{}
		f := runFailer(t, log.deps())
		start := time.Now()
		want := make([]string, 0, 500)
		for i := range 500 {
			key := fmt.Sprintf("k%03d", i)
			want = append(want, key)
			f.wantGrab(pendingGrab(key, "rd", start.Add(time.Duration(i)*time.Millisecond)))
		}
		synctest.Wait()
		if got := log.grabKeys(); !slices.Equal(got, want) || f.itemCount() != 0 {
			t.Fatalf("attempts = %d (in order: %v), items = %d; want 500 once each, oldest first, none left",
				len(got), slices.Equal(got, want), f.itemCount())
		}
	})
}

func TestArrFailerAckOnlyResend(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		acks := 0
		log := &attemptLog{}
		log.grab = func(_ context.Context, _ attemptSpec) attemptResult {
			return attemptResult{kind: resAckFailed, jobID: "job-7", err: errors.New("save failed")}
		}
		log.ack = func(_ context.Context, spec attemptSpec) attemptResult {
			acks++
			if acks < 3 {
				return attemptResult{kind: resAckFailed, jobID: spec.jobID, err: errors.New("save failed")}
			}
			return attemptResult{kind: resAcked, jobID: spec.jobID}
		}
		f := runFailer(t, log.deps())
		f.wantGrab(pendingGrab("a", "rd", start))
		time.Sleep(time.Hour)
		synctest.Wait()
		if log.grabCount() != 1 || log.ackCount() != 3 || f.itemCount() != 0 {
			t.Fatalf("grab attempts = %d, ack attempts = %d, items = %d; want one grab, three acks, none left",
				log.grabCount(), log.ackCount(), f.itemCount())
		}
		for _, spec := range log.acks {
			if spec.jobID != "job-7" {
				t.Fatalf("ack carried job %q, want job-7", spec.jobID)
			}
		}
	})
}

func TestArrFailerAckIgnoresAHeldBucket(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var acked atomic.Bool
		log := &attemptLog{}
		log.grab = func(_ context.Context, spec attemptSpec) attemptResult {
			switch spec.key {
			case "a":
				return attemptResult{kind: resAckFailed, jobID: "job-1", err: errors.New("save failed")}
			default:
				return attemptResult{kind: resHeld, retryAt: time.Now().Add(time.Hour)}
			}
		}
		log.ack = func(_ context.Context, spec attemptSpec) attemptResult {
			acked.Store(true)
			return attemptResult{kind: resAcked, jobID: spec.jobID}
		}
		f := runFailer(t, log.deps())
		f.wantGrab(pendingGrab("a", "rd", start))
		synctest.Wait()
		f.wantGrab(pendingGrab("b", "rd", start.Add(time.Second)))
		synctest.Wait()
		if _, queued := f.item("b"); !queued {
			t.Fatal("b was not queued behind the running attempt")
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if !acked.Load() {
			t.Fatal("the ack waited for the bucket's hold; it must not, the Arr was already told")
		}
	})
}

func TestArrFailerAttemptPanics(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		panics := true
		log := &attemptLog{}
		log.grab = func(_ context.Context, _ attemptSpec) attemptResult {
			log.mu.Lock()
			defer log.mu.Unlock()
			if panics {
				panic("boom")
			}
			return attemptResult{kind: resAcked}
		}
		f := runFailer(t, log.deps())
		f.wantGrab(pendingGrab("a", "rd", start))
		synctest.Wait()
		it, ok := f.item("a")
		if !ok || it.phase != failWaiting || it.fails != 1 || !strings.Contains(it.lastErr, "boom") {
			t.Fatalf("item = %+v, %v; want waiting with one failure naming the panic", it, ok)
		}
		log.mu.Lock()
		panics = false
		log.mu.Unlock()
		time.Sleep(time.Minute)
		synctest.Wait()
		if f.itemCount() != 0 || log.grabCount() != 2 {
			t.Fatalf("items = %d, attempts = %d; want the retry delivered", f.itemCount(), log.grabCount())
		}
	})
}

func TestArrFailerStaleHeldStillHoldsBucket(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		hold := start.Add(5 * time.Minute)
		started := make(chan struct{})
		release := make(chan struct{})
		log := &attemptLog{}
		log.grab = func(_ context.Context, spec attemptSpec) attemptResult {
			if spec.at.Equal(start) {
				close(started)
				<-release
				return attemptResult{kind: resHeld, retryAt: hold}
			}
			return attemptResult{kind: resAcked}
		}
		f := runFailer(t, log.deps())
		f.wantGrab(pendingGrab("a", "rd", start))
		<-started
		f.wantGrab(pendingGrab("a", "rd", start.Add(time.Second)))
		synctest.Wait()
		close(release)
		synctest.Wait()
		if got := f.heldUntil("sonarr/rd"); !got.Equal(hold) {
			t.Fatalf("heldUntil = %v, want %v even though the attempt was superseded", got, hold)
		}
		if log.grabCount() != 1 {
			t.Fatalf("attempts = %d, want the new occurrence held until %v", log.grabCount(), hold)
		}
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if log.grabCount() != 2 || f.itemCount() != 0 {
			t.Fatalf("attempts = %d, items = %d; want the new occurrence delivered after the hold",
				log.grabCount(), f.itemCount())
		}
	})
}

func TestArrFailerRefailDuringAttempt(t *testing.T) {
	t.Parallel()
	for name, first := range map[string]attemptResult{
		"stale ack of the old occurrence": {kind: resAcked},
		"error of the old occurrence":     {kind: resError, err: errors.New("arr down")},
		"stale":                           {kind: resStale},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				started := make(chan struct{})
				release := make(chan struct{})
				log := &attemptLog{}
				log.grab = func(_ context.Context, spec attemptSpec) attemptResult {
					if spec.at.Equal(start) {
						close(started)
						<-release
						return first
					}
					return attemptResult{kind: resAcked}
				}
				f := runFailer(t, log.deps())
				f.wantGrab(pendingGrab("a", "rd", start))
				<-started
				f.wantGrab(pendingGrab("a", "rd", start.Add(time.Second)))
				close(release)
				synctest.Wait()
				log.mu.Lock()
				defer log.mu.Unlock()
				if len(log.grabs) != 2 || !log.grabs[1].at.Equal(start.Add(time.Second)) || f.itemCount() != 0 {
					t.Fatalf(
						"attempts = %+v, items = %d; want the new occurrence delivered at once",
						log.grabs,
						f.itemCount(),
					)
				}
			})
		})
	}
}

func TestArrFailerUntell(t *testing.T) {
	t.Parallel()
	t.Run("waiting item in a held bucket", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			log := &attemptLog{}
			log.grab = func(_ context.Context, _ attemptSpec) attemptResult {
				return attemptResult{kind: resHeld, retryAt: time.Now().Add(time.Hour)}
			}
			f := runFailer(t, log.deps())
			f.wantGrab(pendingGrab("a", "rd", start))
			synctest.Wait()
			f.untell("a", start)
			time.Sleep(2 * time.Hour)
			synctest.Wait()
			if log.grabCount() != 1 || f.itemCount() != 0 {
				t.Fatalf("attempts = %d, items = %d; want the untold item dropped and never retried",
					log.grabCount(), f.itemCount())
			}
		})
	})
	t.Run("running item", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			started := make(chan struct{})
			release := make(chan struct{})
			log := &attemptLog{}
			log.grab = func(_ context.Context, _ attemptSpec) attemptResult {
				close(started)
				<-release
				return attemptResult{kind: resError, err: errors.New("arr down")}
			}
			f := runFailer(t, log.deps())
			f.wantGrab(pendingGrab("a", "rd", start))
			<-started
			f.untell("a", start)
			close(release)
			time.Sleep(time.Hour)
			synctest.Wait()
			if log.grabCount() != 1 || f.itemCount() != 0 {
				t.Fatalf("attempts = %d, items = %d; want the untold item dropped", log.grabCount(), f.itemCount())
			}
		})
	})
	t.Run("another occurrence", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			log := &attemptLog{}
			log.grab = func(_ context.Context, _ attemptSpec) attemptResult {
				return attemptResult{kind: resError, err: errors.New("arr down")}
			}
			f := runFailer(t, log.deps())
			f.wantGrab(pendingGrab("a", "rd", start))
			synctest.Wait()
			f.untell("a", start.Add(time.Second))
			if _, ok := f.item("a"); !ok {
				t.Fatal("an untell of another occurrence dropped the item")
			}
		})
	})
}

func TestArrFailerWaitsForArrRegistration(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		registered := false
		log := &attemptLog{}
		log.grab = func(_ context.Context, _ attemptSpec) attemptResult {
			log.mu.Lock()
			defer log.mu.Unlock()
			if !registered {
				return attemptResult{kind: resNoArr}
			}
			return attemptResult{kind: resAcked}
		}
		f := runFailer(t, log.deps())
		f.wantGrab(pendingGrab("a", "rd", start))
		time.Sleep(48 * time.Hour)
		synctest.Wait()
		if log.grabCount() != 1 {
			t.Fatalf("attempts = %d, want the item parked", log.grabCount())
		}
		if st, _ := f.status("a", start); st.Waiting != failWaitArrStatus {
			t.Fatalf("status = %+v, want waiting for the arr", st)
		}
		log.mu.Lock()
		registered = true
		log.mu.Unlock()
		f.arrAdded("radarr")
		synctest.Wait()
		if log.grabCount() != 1 {
			t.Fatal("another arr's registration woke the item")
		}
		f.arrAdded("sonarr")
		synctest.Wait()
		if log.grabCount() != 2 || f.itemCount() != 0 {
			t.Fatalf("attempts = %d, items = %d; want delivery after the arr was added", log.grabCount(), f.itemCount())
		}
	})
}

func TestArrFailerWaitsForArrRegistrationDuringTheAttempt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		started := make(chan struct{})
		release := make(chan struct{})
		log := &attemptLog{}
		log.grab = func(_ context.Context, _ attemptSpec) attemptResult {
			log.mu.Lock()
			n := len(log.grabs)
			log.mu.Unlock()
			if n == 1 {
				close(started)
				<-release
				return attemptResult{kind: resNoArr}
			}
			return attemptResult{kind: resAcked}
		}
		f := runFailer(t, log.deps())
		f.wantGrab(pendingGrab("a", "rd", start))
		<-started
		f.arrAdded("sonarr")
		close(release)
		synctest.Wait()
		if log.grabCount() != 2 || f.itemCount() != 0 {
			t.Fatalf("attempts = %d, items = %d; want a retry, not a park, after a registration mid-attempt",
				log.grabCount(), f.itemCount())
		}
	})
}

func TestArrFailerOtherJobWaitsForRelease(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		free := false
		log := &attemptLog{}
		log.grab = func(_ context.Context, _ attemptSpec) attemptResult {
			log.mu.Lock()
			defer log.mu.Unlock()
			if !free {
				return attemptResult{kind: resOtherJob, err: errors.New("another job")}
			}
			return attemptResult{kind: resAcked}
		}
		f := runFailer(t, log.deps())
		f.wantGrab(pendingGrab("a", "rd", start))
		time.Sleep(48 * time.Hour)
		synctest.Wait()
		if st, _ := f.status("a", start); log.grabCount() != 1 || st.Waiting != failWaitKeyStatus {
			t.Fatalf("attempts = %d, status = %+v; want parked on the other job", log.grabCount(), st)
		}
		log.mu.Lock()
		free = true
		log.mu.Unlock()
		f.keyReleased("sonarr", "B")
		synctest.Wait()
		if log.grabCount() != 1 {
			t.Fatal("the release of another key woke the item")
		}
		f.keyReleased("sonarr", "A")
		synctest.Wait()
		if log.grabCount() != 2 || f.itemCount() != 0 {
			t.Fatalf("attempts = %d, items = %d; want delivery after the release", log.grabCount(), f.itemCount())
		}
	})
}

func TestArrFailerOtherJobWaitsForReleaseDuringTheAttempt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		started := make(chan struct{})
		release := make(chan struct{})
		log := &attemptLog{}
		log.grab = func(_ context.Context, _ attemptSpec) attemptResult {
			log.mu.Lock()
			n := len(log.grabs)
			log.mu.Unlock()
			if n == 1 {
				close(started)
				<-release
				return attemptResult{kind: resOtherJob, err: errors.New("another job")}
			}
			return attemptResult{kind: resAcked}
		}
		f := runFailer(t, log.deps())
		f.wantGrab(pendingGrab("a", "rd", start))
		<-started
		f.keyReleased("sonarr", "A")
		close(release)
		synctest.Wait()
		if log.grabCount() != 2 || f.itemCount() != 0 {
			t.Fatalf("attempts = %d, items = %d; want a retry after a release mid-attempt",
				log.grabCount(), f.itemCount())
		}
	})
}

func TestArrFailerStartRefused(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		g := testGroup(t)
		var refuse atomic.Bool
		refuse.Store(true)
		var starts atomic.Int64
		log := &attemptLog{}
		deps := log.deps()
		deps.startAttempt = func(name string, fn func(ctx context.Context)) error {
			starts.Add(1)
			if refuse.Load() {
				return errors.New("stopping")
			}
			return g.Go(name, func(ctx context.Context) error {
				fn(ctx)
				return nil
			})
		}
		f := runFailer(t, deps)
		synctest.Wait()
		f.wantGrab(pendingGrab("a", "rd", start))
		synctest.Wait()
		it, ok := f.item("a")
		if !ok || it.phase != failWaiting || it.fails != 1 || it.lastErr != "stopping" {
			t.Fatalf("item = %+v, %v; want waiting, one failure and the refusal as the error", it, ok)
		}
		if bucket := f.heldUntil("sonarr/rd"); !bucket.IsZero() {
			t.Fatalf("heldUntil = %v, a refused start must not hold the bucket", bucket)
		}
		refuse.Store(false)
		time.Sleep(time.Minute)
		synctest.Wait()
		if log.grabCount() != 1 || f.itemCount() != 0 {
			t.Fatalf("attempts = %d, items = %d; want delivery on the next try", log.grabCount(), f.itemCount())
		}
		if got := starts.Load(); got != 2 {
			t.Fatalf("starts = %d, want the refused one and the retry after the backoff", got)
		}
	})
}

func TestArrFailerRecheckPrecedence(t *testing.T) {
	t.Parallel()
	entry := &storage.Entry{InfoHash: "A", ActiveProvider: "rd"}
	t.Run("a persisted item keeps its place", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			release := make(chan struct{})
			log := &attemptLog{grab: func(_ context.Context, _ attemptSpec) attemptResult {
				<-release
				return attemptResult{kind: resAcked}
			}}
			f := runFailer(t, log.deps())
			f.wantGrab(pendingGrab("a", "rd", start))
			synctest.Wait()
			f.wantRecheck(entry, "sonarr")
			it, _ := f.item("a")
			if it.recheck {
				t.Fatal("a recheck replaced a persisted fail")
			}
			close(release)
			synctest.Wait()
			if len(log.rechecks) != 0 {
				t.Fatalf("rechecks = %d, want none", len(log.rechecks))
			}
		})
	})
	t.Run("a persisted fail replaces a waiting recheck", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			log := &attemptLog{recheck: func(_ context.Context, _ attemptSpec) attemptResult {
				return attemptResult{kind: resError, err: errors.New("arr down")}
			}}
			f := runFailer(t, log.deps())
			f.wantRecheck(entry, "sonarr")
			synctest.Wait()
			f.wantGrab(pendingGrab("a", "rd", start))
			synctest.Wait()
			if log.grabCount() != 1 || f.itemCount() != 0 {
				t.Fatalf(
					"grab attempts = %d, items = %d; want the persisted fail delivered",
					log.grabCount(),
					f.itemCount(),
				)
			}
		})
	})
	t.Run("a recheck is given up after repeated errors", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			log := &attemptLog{recheck: func(_ context.Context, _ attemptSpec) attemptResult {
				return attemptResult{kind: resError, err: errors.New("arr down")}
			}}
			f := runFailer(t, log.deps())
			f.wantRecheck(entry, "sonarr")
			time.Sleep(24 * time.Hour)
			synctest.Wait()
			log.mu.Lock()
			defer log.mu.Unlock()
			if len(log.rechecks) != maxRecheckAttempts || f.itemCount() != 0 {
				t.Fatalf("rechecks = %d, items = %d; want %d tries then given up",
					len(log.rechecks), f.itemCount(), maxRecheckAttempts)
			}
		})
	})
}

func TestArrFailerRecheckMoves(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		log := &attemptLog{}
		log.recheck = func(_ context.Context, _ attemptSpec) attemptResult {
			log.mu.Lock()
			n := len(log.rechecks)
			log.mu.Unlock()
			if n == 1 {
				return attemptResult{kind: resMoved, arr: "radarr", provider: "other"}
			}
			return attemptResult{kind: resAcked}
		}
		f := runFailer(t, log.deps())
		f.wantRecheck(&storage.Entry{InfoHash: "A", ActiveProvider: "rd"}, "sonarr")
		synctest.Wait()
		log.mu.Lock()
		defer log.mu.Unlock()
		if len(log.rechecks) != 2 || log.rechecks[1].bucket() != "radarr/other" || f.itemCount() != 0 {
			t.Fatalf("rechecks = %+v, items = %d; want a second try in the new bucket", log.rechecks, f.itemCount())
		}
	})
}

func TestArrFailerStatusShowsTheHold(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		hold := start.Add(30 * time.Minute)
		log := &attemptLog{grab: func(_ context.Context, _ attemptSpec) attemptResult {
			return attemptResult{kind: resHeld, retryAt: hold, err: errors.New("held")}
		}}
		f := runFailer(t, log.deps())
		f.wantGrab(pendingGrab("a", "rd", start))
		synctest.Wait()
		st, ok := f.status("a", start)
		if !ok || !st.NextRetry.Equal(hold) || st.LastError != "held" {
			t.Fatalf("status = %+v, %v; want the retry at the end of the hold", st, ok)
		}
		if _, answered := f.status("a", start.Add(time.Second)); answered {
			t.Fatal("status answered for another occurrence")
		}
		if _, answered := f.status("zzz", start); answered {
			t.Fatal("status answered for another key")
		}
	})
}

func deliverOnly(m *Manager, recovery ArrRecovery) func(context.Context, attemptSpec) attemptResult {
	return func(ctx context.Context, spec attemptSpec) attemptResult {
		_, err := m.deliverFail(ctx, recovery, failDelivery{
			arr: spec.arr, provider: spec.provider, downloadID: strings.ToUpper(spec.key), entryID: spec.key,
			since: spec.at,
		})
		if err != nil {
			return failResultOf(attemptResult{spec: spec}, err)
		}
		return attemptResult{spec: spec, kind: resAcked}
	}
}

func failsOf(f *fakeArrRecovery) [][3]string {
	f.failMu.Lock()
	defer f.failMu.Unlock()
	return slices.Clone(f.fails)
}

func (m *Manager) testFailer(t testing.TB, wrap ...func(*arrFailerDeps)) *arrFailer {
	t.Helper()
	deps := m.failerDeps(zerolog.Nop())
	for _, w := range wrap {
		w(&deps)
	}
	f := newArrFailer(deps)
	m.failer = f
	if err := m.work.Go("arr-failer", f.run); err != nil {
		t.Fatal(err)
	}
	return f
}

func (a *attemptLog) sortedGrabKeys() []string {
	keys := a.grabKeys()
	slices.Sort(keys)
	return keys
}

func TestArrFailerStaleResultCannotTouchARunningAttempt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		started := make(chan struct{})
		release := make(chan struct{})
		log := &attemptLog{}
		log.grab = func(_ context.Context, spec attemptSpec) attemptResult {
			if spec.key == "a" {
				close(started)
				<-release
			}
			return attemptResult{kind: resAcked}
		}
		f := runFailer(t, log.deps())
		f.wantGrab(pendingGrab("a", "rd", start))
		<-started
		running, ok := f.item("a")
		if !ok || running.phase != failRunning {
			t.Fatalf("item = %+v, %v; want it running", running, ok)
		}
		f.apply(attemptResult{
			spec: attemptSpec{key: "a", arr: "sonarr", provider: "rd", at: start, attempt: running.attempt - 1},
			kind: resAcked,
		})
		if it, present := f.item("a"); !present || it.phase != failRunning {
			t.Fatalf("item = %+v, %v; want a stale result to leave the running attempt's item alone", it, present)
		}
		f.wantGrab(pendingGrab("b", "rd", start.Add(time.Second)))
		synctest.Wait()
		if got := log.grabKeys(); !slices.Equal(got, []string{"a"}) {
			t.Fatalf("attempts = %v, want the bucket still held by a's running attempt", got)
		}
		close(release)
		synctest.Wait()
		if got := log.grabKeys(); !slices.Equal(got, []string{"a", "b"}) || f.itemCount() != 0 {
			t.Fatalf("attempts = %v, items = %d; want b after a finished", got, f.itemCount())
		}
	})
}

func TestArrFailerRefusedAckLaunchKeepsAckWaiting(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		g := testGroup(t)
		var refuse atomic.Bool
		log := &attemptLog{}
		log.grab = func(_ context.Context, _ attemptSpec) attemptResult {
			refuse.Store(true)
			return attemptResult{kind: resAckFailed, jobID: "job-7", err: errors.New("save failed")}
		}
		deps := log.deps()
		deps.startAttempt = func(name string, fn func(ctx context.Context)) error {
			if refuse.Load() {
				return errors.New("stopping")
			}
			return g.Go(name, func(ctx context.Context) error {
				fn(ctx)
				return nil
			})
		}
		f := runFailer(t, deps)
		synctest.Wait()
		f.wantGrab(pendingGrab("a", "rd", start))
		time.Sleep(time.Minute)
		synctest.Wait()
		it, ok := f.item("a")
		if !ok || it.phase != failAckWaiting || it.jobID != "job-7" || it.lastErr != "stopping" {
			t.Fatalf("item = %+v, %v; want ack-waiting with job-7 and the refusal as the error", it, ok)
		}
		refuse.Store(false)
		time.Sleep(time.Hour)
		synctest.Wait()
		if log.grabCount() != 1 || log.ackCount() != 1 || f.itemCount() != 0 {
			t.Fatalf("grabs = %d, acks = %d, items = %d; want one grab, one ack, none left",
				log.grabCount(), log.ackCount(), f.itemCount())
		}
		if log.acks[0].jobID != "job-7" {
			t.Fatalf("ack carried job %q, want job-7", log.acks[0].jobID)
		}
	})
}

func TestArrFailerRefusedLaunchOfAnUntoldItemDropsIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		log := &attemptLog{}
		deps := log.deps()
		var f *arrFailer
		deps.startAttempt = func(string, func(ctx context.Context)) error {
			f.untell("a", start)
			return errors.New("stopping")
		}
		f = runFailer(t, deps)
		synctest.Wait()
		f.wantGrab(pendingGrab("a", "rd", start))
		synctest.Wait()
		if f.itemCount() != 0 || log.grabCount() != 0 {
			t.Fatalf("items = %d, attempts = %d; want the untold item dropped", f.itemCount(), log.grabCount())
		}
	})
}

func TestArrFailerAckAttemptPanicKeepsAckWaiting(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		log := &attemptLog{}
		log.grab = func(_ context.Context, _ attemptSpec) attemptResult {
			return attemptResult{kind: resAckFailed, jobID: "job-7", err: errors.New("save failed")}
		}
		log.ack = func(_ context.Context, _ attemptSpec) attemptResult { panic("boom") }
		f := runFailer(t, log.deps())
		f.wantGrab(pendingGrab("a", "rd", start))
		time.Sleep(time.Minute)
		synctest.Wait()
		it, ok := f.item("a")
		if !ok || it.phase != failAckWaiting || it.jobID != "job-7" || !strings.Contains(it.lastErr, "boom") {
			t.Fatalf("item = %+v, %v; want ack-waiting with job-7 after the panic", it, ok)
		}
	})
}

func TestArrFailerHeldFromAnotherAttemptDoesNotHoldTheBucket(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		started := make(chan struct{})
		release := make(chan struct{})
		log := &attemptLog{}
		log.grab = func(_ context.Context, _ attemptSpec) attemptResult {
			close(started)
			<-release
			return attemptResult{kind: resAcked}
		}
		f := runFailer(t, log.deps())
		f.wantGrab(pendingGrab("a", "rd", start))
		<-started
		running, _ := f.item("a")
		f.apply(attemptResult{
			spec: attemptSpec{key: "a", arr: "sonarr", provider: "rd", at: start, attempt: running.attempt - 1},
			kind: resHeld, retryAt: start.Add(time.Hour),
		})
		if got := f.heldUntil("sonarr/rd"); !got.IsZero() {
			t.Fatalf("heldUntil = %v, want a result of an earlier attempt to leave the bucket alone", got)
		}
		close(release)
		synctest.Wait()
	})
}

func TestArrFailerSupersedeClearsTheOldStatus(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		second := start.Add(time.Second)
		started := make(chan struct{})
		release := make(chan struct{})
		log := &attemptLog{}
		log.grab = func(_ context.Context, spec attemptSpec) attemptResult {
			if spec.attempt == 1 {
				return attemptResult{kind: resError, err: errors.New("arr down")}
			}
			close(started)
			<-release
			return attemptResult{kind: resAcked}
		}
		f := runFailer(t, log.deps())
		f.wantGrab(pendingGrab("a", "rd", start))
		time.Sleep(time.Minute)
		<-started
		if st, _ := f.status("a", start); st.LastError != "arr down" {
			t.Fatalf("status = %+v, want the first occurrence to show its error", st)
		}
		f.wantGrab(pendingGrab("a", "rd", second))
		st, ok := f.status("a", second)
		if !ok || st.LastError != "" {
			t.Fatalf("status = %+v, %v; want the new occurrence to show no old error", st, ok)
		}
		close(release)
		synctest.Wait()
	})
}
