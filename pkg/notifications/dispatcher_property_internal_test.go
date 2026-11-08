package notifications

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/proptest"
	"github.com/sirrobot01/decypharr/internal/task"
)

// pout is one scripted send outcome of the property test.
type pout uint8

const (
	poutOK pout = iota
	pout503
	pout429
	pout429Wait3
	pout429Wait45
	pout400
	poutNetwork
	poutHang // blocks until the send times out
	poutPanic
)

// poutKinds is how many outcomes there are, for drawing one.
const poutKinds = int(poutPanic) + 1

func (o pout) String() string {
	return [...]string{"ok", "503", "429", "429/ra3", "429/ra45", "400", "net", "hang", "panic"}[o]
}

type popKind uint8

const (
	popNotify popKind = iota
	popBurst
	popAdvance
)

// pop is one step: notify one event with a script, notify a burst of plain
// events, or let time pass.
type pop struct {
	kind     popKind
	notifier int
	script   []pout
	count    int
	advance  time.Duration
}

func (p pop) String() string {
	switch p.kind {
	case popNotify:
		return fmt.Sprintf("notify(n%d %v)", p.notifier, p.script)
	case popBurst:
		return fmt.Sprintf("burst(n%d ×%d)", p.notifier, p.count)
	case popAdvance:
		return fmt.Sprintf("advance(%v)", p.advance)
	default:
		return "unknown"
	}
}

const (
	propNotifiers  = 2
	propMaxOps     = 40
	propBurstMin   = 95
	propBurstRange = 16
)

func genPops(seed int64) []pop {
	rng := rand.New(rand.NewPCG(uint64(seed), uint64(seed)+11))
	ops := make([]pop, 4+rng.IntN(propMaxOps))
	advances := []time.Duration{
		500 * time.Millisecond,
		time.Second,
		5 * time.Second,
		10 * time.Second,
		30 * time.Second,
	}
	for i := range ops {
		n := rng.IntN(propNotifiers)
		switch r := rng.IntN(100); {
		case r < 55:
			script := make([]pout, rng.IntN(maxSends+1))
			for j := range script {
				script[j] = pout(rng.IntN(poutKinds))
			}
			ops[i] = pop{kind: popNotify, notifier: n, script: script}
		case r < 60:
			ops[i] = pop{kind: popBurst, notifier: n, count: propBurstMin + rng.IntN(propBurstRange)}
		default:
			ops[i] = pop{kind: popAdvance, advance: advances[rng.IntN(len(advances))]}
		}
	}
	return ops
}

func simplifyPop(p pop) []pop {
	var simpler []pop
	if p.notifier != 0 {
		q := p
		q.notifier = 0
		simpler = append(simpler, q)
	}
	for i := range p.script {
		q := p
		q.script = slices.Delete(slices.Clone(p.script), i, i+1)
		simpler = append(simpler, q)
	}
	if p.kind == popBurst && p.count > 1 {
		q := p
		q.count = p.count / 2
		simpler = append(simpler, q)
	}
	return simpler
}

// propEvent is what the replay knows of one notified event.
type propEvent struct {
	seq      int
	notifier string
	script   []pout
	sends    []time.Time
	// firstSend is the index of its first send among its notifier's sends.
	firstSend int
	outcomes  []string // "delivered" or "dropped:<reason>/<attempts>"
}

// propWorld is the fake notifiers' shared record.
type propWorld struct {
	mu       sync.Mutex
	events   map[string]*propEvent
	inFlight map[string]int
	// sendCount counts each notifier's sends, so send order is known even
	// when sends share a synctest instant.
	sendCount map[string]int
	overlaps  []string
}

type propNotifier struct {
	name  string
	world *propWorld
}

func (n *propNotifier) Name() string { return n.name }

func (n *propNotifier) Send(ctx context.Context, ev Event) error {
	w := n.world
	w.mu.Lock()
	e := w.events[ev.Message]
	k := len(e.sends)
	if k == 0 {
		e.firstSend = w.sendCount[n.name]
	}
	w.sendCount[n.name]++
	e.sends = append(e.sends, time.Now())
	w.inFlight[n.name]++
	if w.inFlight[n.name] > 1 {
		w.overlaps = append(w.overlaps, ev.Message)
	}
	o := poutOK
	if k < len(e.script) {
		o = e.script[k]
	}
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.inFlight[n.name]--
		w.mu.Unlock()
	}()
	switch o {
	case poutOK:
		return nil
	case pout503:
		return &StatusError{Code: http.StatusServiceUnavailable}
	case pout429:
		return &StatusError{Code: http.StatusTooManyRequests}
	case pout429Wait3:
		return &StatusError{Code: http.StatusTooManyRequests, RetryAfter: 3 * time.Second}
	case pout429Wait45:
		return &StatusError{Code: http.StatusTooManyRequests, RetryAfter: 45 * time.Second}
	case pout400:
		return &StatusError{Code: http.StatusBadRequest}
	case poutNetwork:
		return errors.New("connection refused")
	case poutHang:
		<-ctx.Done()
		return fmt.Errorf("post: %w", ctx.Err())
	case poutPanic:
		panic("scripted panic")
	default:
		return fmt.Errorf("unknown outcome %d", o)
	}
}

func (w *propWorld) delivered(_ string, ev Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e := w.events[ev.Message]
	e.outcomes = append(e.outcomes, "delivered")
}

func (w *propWorld) dropped(_ string, ev Event, reason dropReason, attempts int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e := w.events[ev.Message]
	e.outcomes = append(e.outcomes, fmt.Sprintf("dropped:%v/%d", reason, attempts))
}

// expect is the model of one event that was not pushed out of a full queue:
// its outcome and the gap before each send after the first, worked out from
// SM06-4 by hand (not by calling decide).
func expect(script []pout) (string, []time.Duration) {
	waits := []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}
	var gaps []time.Duration
	for k := range maxSends {
		o := poutOK
		if k < len(script) {
			o = script[k]
		}
		var took, floor time.Duration
		switch o {
		case poutOK:
			return "delivered", gaps
		case pout400, poutPanic:
			return fmt.Sprintf("dropped:rejected/%d", k+1), gaps
		case pout429Wait3:
			floor = 3 * time.Second
		case pout429Wait45:
			floor = 30 * time.Second
		case poutHang:
			took = sendTimeout
		case pout503, pout429, poutNetwork:
		}
		if k == maxSends-1 {
			return fmt.Sprintf("dropped:failed/%d", k+1), gaps
		}
		gaps = append(gaps, took+max(waits[k], floor))
	}
	panic("unreachable")
}

// dispatcherViolations replays ops against a real runner under synctest and
// returns what broke. A [synctest.Wait] after every step makes the outcome a
// pure function of ops.
func dispatcherViolations(t *testing.T, ops []pop) []string {
	var broken []string
	synctest.Test(t, func(t *testing.T) {
		world := &propWorld{
			events:    map[string]*propEvent{},
			inFlight:  map[string]int{},
			sendCount: map[string]int{},
		}
		var notifiers []Notifier
		for i := range propNotifiers {
			notifiers = append(notifiers, &propNotifier{name: "n" + strconv.Itoa(i), world: world})
		}
		g := task.NewRoot(context.WithoutCancel(t.Context()), "prop", zerolog.Nop())
		r := newRunner(g, notifiers, zerolog.Nop(), world)
		seq := 0
		notify := func(notifier int, script []pout) {
			seq++
			id := strconv.Itoa(seq)
			name := "n" + strconv.Itoa(notifier)
			world.mu.Lock()
			world.events[id] = &propEvent{seq: seq, notifier: name, script: script}
			world.mu.Unlock()
			if err := r.Send(name, msg{kind: msgEnqueue, ev: Event{Message: id}}); err != nil {
				broken = append(broken, fmt.Sprintf("event %s refused: %v", id, err))
			}
		}
		for _, o := range ops {
			switch o.kind {
			case popNotify:
				notify(o.notifier, o.script)
			case popBurst:
				for range o.count {
					notify(o.notifier, nil)
				}
			case popAdvance:
				time.Sleep(o.advance)
			}
			synctest.Wait()
		}
		time.Sleep(24 * time.Hour) // every event settles
		synctest.Wait()
		broken = append(broken, checkWorld(world)...)
		ctx, cancel := task.Detached(t.Context(), time.Minute)
		defer cancel()
		if err := g.Stop(ctx); err != nil {
			broken = append(broken, fmt.Sprintf("stop: %v", err))
		}
	})
	return broken
}

// order is what the check carries from one event of a notifier to the next.
type order struct {
	// minFirstSend is the lowest send index the next sent event may start at.
	minFirstSend int
	lastOverflow int
}

func checkWorld(w *propWorld) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var broken []string
	for _, id := range w.overlaps {
		broken = append(broken, "event "+id+" was sent while another send of its notifier ran")
	}
	events := make([]*propEvent, 0, len(w.events))
	for _, e := range w.events {
		events = append(events, e)
	}
	slices.SortFunc(events, func(a, b *propEvent) int { return a.seq - b.seq })
	orders := map[string]*order{}
	for _, e := range events {
		o := orders[e.notifier]
		if o == nil {
			o = &order{}
			orders[e.notifier] = o
		}
		broken = append(broken, checkEvent(e, o)...)
	}
	return broken
}

// checkEvent holds one event to its model: ended exactly once; pushed out of
// a full queue unsent and oldest first; or sent as often, as far apart and
// with the outcome expect says, after the older events of its notifier.
func checkEvent(e *propEvent, o *order) []string {
	if len(e.outcomes) != 1 {
		return []string{fmt.Sprintf("event %d ended %d times: %v", e.seq, len(e.outcomes), e.outcomes)}
	}
	var broken []string
	if e.outcomes[0] == "dropped:overflow/0" {
		if len(e.sends) != 0 {
			broken = append(broken, fmt.Sprintf("event %d was sent and then pushed out of the queue", e.seq))
		}
		if e.seq < o.lastOverflow {
			broken = append(broken, fmt.Sprintf("event %d pushed out after a newer one", e.seq))
		}
		o.lastOverflow = e.seq
		return broken
	}
	outcome, gaps := expect(e.script)
	if e.outcomes[0] != outcome {
		broken = append(broken, fmt.Sprintf("event %d %v: ended %s, want %s", e.seq, e.script, e.outcomes[0], outcome))
	}
	if len(e.sends) != len(gaps)+1 {
		return append(broken, fmt.Sprintf("event %d %v: %d sends, want %d", e.seq, e.script, len(e.sends), len(gaps)+1))
	}
	for k, gap := range gaps {
		if got := e.sends[k+1].Sub(e.sends[k]); got != gap {
			broken = append(broken, fmt.Sprintf("event %d %v: send %d came %v after the one before, want %v",
				e.seq, e.script, k+2, got, gap))
		}
	}
	if e.firstSend < o.minFirstSend {
		broken = append(broken, fmt.Sprintf("event %d was first sent before an older event of its notifier", e.seq))
	}
	o.minFirstSend = e.firstSend + 1
	return broken
}

// TestDispatcherPropertyOrderWithoutDelays: events queued behind a head that
// all deliver at once are first sent in notify order, though every send
// happens at the same synctest instant.
func TestDispatcherPropertyOrderWithoutDelays(t *testing.T) {
	t.Parallel()
	ops := []pop{{kind: popBurst, notifier: 0, count: 3}}
	if broken := dispatcherViolations(t, ops); len(broken) != 0 {
		t.Fatalf("%v: %v", ops, broken)
	}
}

func TestDispatcherProperty(t *testing.T) {
	t.Parallel()
	proptest.CheckWhy(t, proptest.Config{Seeds: 200}, genPops,
		func(ops []pop) []string { return dispatcherViolations(t, ops) }, simplifyPop)
}
