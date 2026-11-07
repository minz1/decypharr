package keyed_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/proptest"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

type opKind int

const (
	opSend opKind = iota + 1
	opAfter
	opCancel
	opAdvance
	opStop
)

// op is one step of a generated sequence. key picks one of propKeys; at is a delay in milliseconds, or the index
// of the timer to cancel.
type op struct {
	kind opKind
	key  int
	at   int
}

func (o op) String() string {
	switch o.kind {
	case opSend:
		return fmt.Sprintf("send(k%d)", o.key)
	case opAfter:
		return fmt.Sprintf("after(k%d,%dms)", o.key, o.at)
	case opCancel:
		return fmt.Sprintf("cancel(#%d)", o.at)
	case opAdvance:
		return fmt.Sprintf("advance(%dms)", o.at)
	case opStop:
		return "stop"
	default:
		return "?"
	}
}

const (
	propKeys = 3
	// timerBase separates the ids of timer messages from those of sends.
	timerBase = 1_000_000
	// propIdle is the idle exit of the runner under test, short so sequences exercise it.
	propIdle = 40 * time.Millisecond
	// propTick is the time every step takes, so the handlers of the step have finished before the next one.
	propTick = time.Millisecond
)

func genOps(seed int64) []op {
	rng := rand.New(rand.NewPCG(uint64(seed), uint64(seed)+7))
	ops := make([]op, 5+rng.IntN(35))
	for i := range ops {
		switch rng.IntN(10) {
		case 0, 1, 2, 3:
			ops[i] = op{kind: opSend, key: rng.IntN(propKeys)}
		case 4, 5:
			ops[i] = op{kind: opAfter, key: rng.IntN(propKeys), at: 1 + rng.IntN(200)}
		case 6:
			ops[i] = op{kind: opCancel, at: rng.IntN(8)}
		case 7, 8:
			ops[i] = op{kind: opAdvance, at: 1 + rng.IntN(120)}
		default:
			if rng.IntN(4) == 0 { // a stop is rarer, so most sequences run long
				ops[i] = op{kind: opStop}
			} else {
				ops[i] = op{kind: opSend, key: rng.IntN(propKeys)}
			}
		}
	}
	return ops
}

func simplifyOp(o op) []op {
	var simpler []op
	if o.key != 0 {
		simpler = append(simpler, op{kind: o.kind, key: 0, at: o.at})
	}
	if o.at > 1 {
		simpler = append(simpler, op{kind: o.kind, key: o.key, at: o.at / 2})
	}
	return simpler
}

// timerModel is what the sequence expects of one timer.
type timerModel struct {
	id        keyed.TimerID
	key       int
	msg       int
	due       time.Duration
	cancelled bool
}

// observer records what the handlers saw.
type observer struct {
	mu      sync.Mutex
	handled map[int][]int // key -> ids in handling order
	active  map[int]int
	overlap bool
	count   int
}

func newObserver() *observer {
	return &observer{handled: map[int][]int{}, active: map[int]int{}}
}

func (o *observer) handle(_ context.Context, key int, m int, _ *keyed.Effects[int, int]) {
	o.mu.Lock()
	o.active[key]++
	if o.active[key] > 1 {
		o.overlap = true
	}
	o.mu.Unlock()
	// Wait inside the handler, so a second handler for the key would overlap this one.
	time.Sleep(time.Microsecond)
	o.mu.Lock()
	o.active[key]--
	o.handled[key] = append(o.handled[key], m)
	o.count++
	o.mu.Unlock()
}

func (o *observer) total() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.count
}

// replay applies a generated sequence to a real runner and keeps the model of what that should have done.
type replay struct {
	t       *testing.T
	root    *task.Group
	runner  *keyed.Runner[int, int, struct{}]
	obs     *observer
	elapsed time.Duration
	stopped bool
	stopAt  time.Duration
	atStop  int
	sent    map[int][]int // key -> ids Send accepted, in order
	timers  []timerModel
	broken  []string
}

func newReplay(t *testing.T) *replay {
	root := task.NewRoot(context.WithoutCancel(t.Context()), "prop", zerolog.Nop())
	obs := newObserver()
	return &replay{
		t:    t,
		root: root,
		obs:  obs,
		runner: keyed.New[int, int, struct{}](root, "prop", keyed.HandlerFunc[int, int](obs.handle),
			keyed.Options[int]{MailboxSize: 1_000, IdleAfter: propIdle}),
		sent: map[int][]int{},
	}
}

func (p *replay) failf(format string, args ...any) {
	p.broken = append(p.broken, fmt.Sprintf(format, args...))
}

// settle lets time pass and the runner finish what it was given.
func (p *replay) settle() {
	time.Sleep(propTick)
	p.elapsed += propTick
	synctest.Wait()
}

func (p *replay) stop() {
	if p.stopped {
		return
	}
	ctx, cancel := task.Detached(p.t.Context(), time.Minute)
	defer cancel()
	if err := p.root.Stop(ctx); err != nil {
		p.failf("Stop: %v", err)
	}
	p.stopped, p.atStop, p.stopAt = true, p.obs.total(), p.elapsed
}

func (p *replay) send(i int, o op) {
	err := p.runner.Send(o.key, i)
	switch {
	case err == nil && p.stopped:
		p.failf("op %d: Send accepted after Stop", i)
	case err == nil:
		p.sent[o.key] = append(p.sent[o.key], i)
	case !p.stopped:
		p.failf("op %d: Send refused with %v", i, err)
	}
}

func (p *replay) after(i int, o op) {
	delay := time.Duration(o.at) * time.Millisecond
	id := p.runner.After(o.key, delay, timerBase+i)
	switch {
	case id == 0 && !p.stopped:
		p.failf("op %d: After returned no timer", i)
	case id != 0:
		p.timers = append(p.timers, timerModel{id: id, key: o.key, msg: timerBase + i, due: p.elapsed + delay})
	}
}

func (p *replay) cancel(o op) {
	if o.at >= len(p.timers) {
		return
	}
	p.runner.CancelTimer(p.timers[o.at].id)
	if p.timers[o.at].due > p.elapsed {
		p.timers[o.at].cancelled = true
	}
}

func (p *replay) step(i int, o op) {
	switch o.kind {
	case opSend:
		p.send(i, o)
	case opAfter:
		p.after(i, o)
	case opCancel:
		p.cancel(o)
	case opAdvance:
		time.Sleep(time.Duration(o.at) * time.Millisecond)
		p.elapsed += time.Duration(o.at) * time.Millisecond
	case opStop:
		p.stop()
	}
	p.settle()
}

// check compares what the handlers saw with what the sequence expects.
func (p *replay) check() {
	p.obs.mu.Lock()
	defer p.obs.mu.Unlock()
	if p.obs.overlap {
		p.failf("two handlers ran at once for one key")
	}
	if p.obs.count != p.atStop {
		p.failf("%d messages were handled after Stop", p.obs.count-p.atStop)
	}
	for key := range propKeys {
		var fromSends []int
		for _, id := range p.obs.handled[key] {
			if id < timerBase {
				fromSends = append(fromSends, id)
			}
		}
		if !slices.Equal(fromSends, p.sent[key]) {
			p.failf("key %d: sends handled %v, accepted %v (order or exactly-once broken)", key, fromSends, p.sent[key])
		}
	}
	p.checkTimers()
}

// checkTimers expects every timer that fell due before the stop, and was not cancelled, to have fired once.
func (p *replay) checkTimers() {
	for _, timer := range p.timers {
		fired := 0
		for _, id := range p.obs.handled[timer.key] {
			if id == timer.msg {
				fired++
			}
		}
		want := 1
		if timer.cancelled || timer.due > p.stopAt {
			want = 0
		}
		if fired != want {
			p.failf("timer for key %d (msg %d) fired %d times, want %d", timer.key, timer.msg, fired, want)
		}
	}
}

// violations replays ops against a real runner and returns what broke. The sequence runs under synctest, and
// every step lets time pass and waits for the runner to settle, so the outcome is a pure function of ops.
func violations(t *testing.T, ops []op) []string {
	var broken []string
	synctest.Test(t, func(t *testing.T) {
		p := newReplay(t)
		for i, o := range ops {
			p.step(i, o)
		}
		if !p.stopped {
			time.Sleep(time.Second) // every pending timer falls due
			p.elapsed += time.Second
			synctest.Wait()
		}
		p.stop()
		time.Sleep(time.Hour)
		synctest.Wait()
		p.check()
		broken = p.broken
	})
	return broken
}

func TestRunnerHoldsItsInvariantsOverRandomSequences(t *testing.T) {
	t.Parallel()
	proptest.Check(t, proptest.Config{Seeds: 60}, genOps,
		func(ops []op) bool { return len(violations(t, ops)) > 0 }, simplifyOp)
}
