package keyed_test

import (
	"context"
	"errors"
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
	opSend      opKind = iota + 1 // a command
	opObserve                     // an observation
	opSendChild                   // a command whose handler starts a child task
	opSendPanic                   // a command whose handler starts a child task that panics
	opAsk                         // an Ask, a command that waits for its reply
	opAfter                       // a timer
	opCancel                      // cancels a timer
	opAdvance                     // lets time pass
	opHold                        // makes the next message of the key block in its handler
	opRelease                     // lets a blocked handler go, and the queue behind it drain
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
	case opObserve:
		return fmt.Sprintf("observe(k%d,group %d)", o.key, o.at)
	case opAsk:
		return fmt.Sprintf("ask(k%d)", o.key)
	case opSendChild:
		return fmt.Sprintf("send+child(k%d)", o.key)
	case opSendPanic:
		return fmt.Sprintf("send+panicking-child(k%d)", o.key)
	case opHold:
		return fmt.Sprintf("hold(k%d)", o.key)
	case opRelease:
		return fmt.Sprintf("release(k%d)", o.key)
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
	// The ids of messages that are not sends are offset, so a handled id says where the message came from.
	timerBase = 1_000_000
	childBase = 2_000_000
	panicBase = 3_000_000
	// propSize is the mailbox bound of the runner under test, small so that holds make mailboxes overflow.
	propSize = 3
	// propIdle is the idle exit of the runner under test, short so sequences exercise it.
	propIdle = 40 * time.Millisecond
	// propTick is the time every step takes, so the handlers of the step have finished before the next one.
	propTick = time.Millisecond
	// propSettle lets the handlers that the last timer of a tick started finish.
	propSettle = 5 * time.Microsecond
	// propSlack is how far from the idle time a gap must be for the state check to expect an outcome.
	propSlack = 3 * time.Millisecond
)

// coalesceGroupCount is how many coalesce keys an observation can carry.
const coalesceGroupCount = 3

// coalesceKey names a group of observations; group 0 has no key and coalesces with nothing.
func coalesceKey(group int) string {
	switch group {
	case 1:
		return "x"
	case 2:
		return "y"
	default:
		return ""
	}
}

func genOps(seed int64) []op {
	rng := rand.New(rand.NewPCG(uint64(seed), uint64(seed)+7))
	ops := make([]op, 8+rng.IntN(50))
	for i := range ops {
		key := rng.IntN(propKeys)
		switch n := rng.IntN(100); {
		case n < 20:
			ops[i] = op{kind: opSend, key: key}
		case n < 36:
			ops[i] = op{kind: opObserve, key: key, at: rng.IntN(coalesceGroupCount)}
		case n < 41:
			ops[i] = op{kind: opSendChild, key: key}
		case n < 46:
			ops[i] = op{kind: opSendPanic, key: key}
		case n < 53:
			ops[i] = op{kind: opAsk, key: key}
		case n < 61:
			ops[i] = op{kind: opAfter, key: key, at: 1 + rng.IntN(60)}
		case n < 69:
			ops[i] = op{kind: opCancel, at: rng.IntN(4)}
		case n < 78:
			ops[i] = op{kind: opAdvance, at: 1 + rng.IntN(120)}
		case n < 87:
			ops[i] = op{kind: opHold, key: key}
		case n < 96:
			ops[i] = op{kind: opRelease, key: key}
		default:
			ops[i] = op{kind: opStop}
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

// pmsg is the message of the runner under test.
type pmsg struct {
	id    int
	obs   bool
	co    string // the coalesce key of an observation
	spawn int    // 0 none, 1 a child task, 2 a child task that panics
}

func classifyProp(m pmsg) keyed.Class {
	if m.obs {
		return keyed.Observation
	}
	return keyed.Command
}

// pstate is the state of a key: which activation built it, and how many messages that activation has handled.
type pstate struct {
	activation int
	handled    int
}

// observer is the handler under test. It records what it saw and checks the state it is given.
type observer struct {
	mu          sync.Mutex
	handled     map[int][]int // key -> ids in handling order
	active      map[int]int
	activations map[int]int // key -> activations so far
	perAct      map[[2]int]int
	lastEnd     map[int]time.Time
	lastAct     map[int]int
	holds       map[int]chan struct{} // armed: the next message of the key blocks on it
	blocked     map[int]chan struct{} // blocking now
	broken      []string
	count       int
}

func newObserver() *observer {
	return &observer{
		handled: map[int][]int{}, active: map[int]int{}, activations: map[int]int{}, perAct: map[[2]int]int{},
		lastEnd: map[int]time.Time{}, lastAct: map[int]int{}, holds: map[int]chan struct{}{},
		blocked: map[int]chan struct{}{},
	}
}

func (o *observer) failf(format string, args ...any) {
	o.broken = append(o.broken, fmt.Sprintf(format, args...))
}

func (o *observer) Activate(_ context.Context, key int) pstate {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.activations[key]++
	return pstate{activation: o.activations[key]}
}

// checkState checks the state the runner handed to a message against the activations and the quiet time.
func (o *observer) checkState(key int, st *pstate) {
	if st.activation != o.activations[key] {
		o.failf("key %d: a message saw the state of activation %d, but activation %d is the latest",
			key, st.activation, o.activations[key])
	}
	slot := [2]int{key, st.activation}
	if st.handled != o.perAct[slot] {
		o.failf("key %d: the state of activation %d had handled %d messages, the activation had handled %d",
			key, st.activation, st.handled, o.perAct[slot])
	}
	o.perAct[slot]++
	if end, seen := o.lastEnd[key]; seen {
		gap := time.Since(end)
		switch {
		case gap > propIdle+propSlack && st.activation == o.lastAct[key]:
			o.failf("key %d: quiet for %v, longer than the idle time, yet the state was kept", key, gap)
		case gap < propIdle-propSlack && st.activation != o.lastAct[key]:
			o.failf("key %d: quiet for only %v, shorter than the idle time, yet the state was dropped", key, gap)
		}
	}
	o.lastAct[key] = st.activation
	st.handled++
}

func (o *observer) Handle(
	ctx context.Context,
	key int,
	st *pstate,
	m pmsg,
	fx *keyed.Effects[int, pmsg],
) {
	o.mu.Lock()
	o.active[key]++
	if o.active[key] > 1 {
		o.failf("two handlers ran at once for key %d", key)
	}
	o.checkState(key, st)
	gate := o.holds[key]
	delete(o.holds, key)
	if gate != nil {
		o.blocked[key] = gate
	}
	o.mu.Unlock()
	// Wait inside the handler, so a second handler for the key would overlap this one.
	time.Sleep(time.Microsecond)
	if gate != nil {
		<-gate
	}
	o.spawn(fx, m)
	fx.Reply(m.id)
	o.mu.Lock()
	o.active[key]--
	o.handled[key] = append(o.handled[key], m.id)
	o.count++
	o.lastEnd[key] = time.Now()
	o.mu.Unlock()
	_ = ctx
}

func (o *observer) spawn(fx *keyed.Effects[int, pmsg], m pmsg) {
	switch m.spawn {
	case 1:
		_, _ = fx.Go("child", func(context.Context) pmsg { return pmsg{id: m.id + childBase} },
			func(any) pmsg { return pmsg{id: m.id + panicBase} })
	case 2:
		_, _ = fx.Go("child", func(context.Context) pmsg { panic("child failed") },
			func(any) pmsg { return pmsg{id: m.id + panicBase} })
	}
}

func (o *observer) arm(key int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.holds[key] == nil && o.blocked[key] == nil {
		o.holds[key] = make(chan struct{})
	}
}

// release lets the blocked handler of key go and disarms the key.
func (o *observer) release(key int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.holds, key)
	if gate := o.blocked[key]; gate != nil {
		delete(o.blocked, key)
		close(gate)
	}
}

func (o *observer) total() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.count
}

// qItem is a message in the model of a mailbox.
type qItem struct {
	id     int
	obs    bool
	co     string
	bypass bool // a timer's message or a child's result
	spawn  int
	timer  int // the id of the timer that delivered it, or 0
}

// keyModel is what the sequence expects of one key: whether a handler is blocked, what waits behind it, and what
// has been handled.
type keyModel struct {
	armed    bool
	blocked  bool
	inflight qItem
	queue    []qItem
	expected []int
}

func (k *keyModel) depth() int {
	n := 0
	for _, item := range k.queue {
		if !item.bypass {
			n++
		}
	}
	return n
}

// queuedWithCoalesceKey is the index of the queued observation an observation with this key replaces, or -1.
func (k *keyModel) queuedWithCoalesceKey(co string) int {
	if co == "" {
		return -1
	}
	return slices.IndexFunc(k.queue, func(item qItem) bool { return item.obs && !item.bypass && item.co == co })
}

// evictOldestObservation removes the oldest queued observation, and reports whether there was one.
func (k *keyModel) evictOldestObservation() bool {
	i := slices.IndexFunc(k.queue, func(item qItem) bool { return item.obs && !item.bypass })
	if i < 0 {
		return false
	}
	k.queue = slices.Delete(k.queue, i, i+1)
	return true
}

// timerModel is what the sequence expects of one timer.
type timerModel struct {
	id        keyed.TimerID
	key       int
	msg       int
	due       time.Duration
	cancelled bool
	fired     bool
}

// replay applies a generated sequence to a real runner and keeps the model of what that should have done.
type replay struct {
	t       *testing.T
	root    *task.Group
	runner  *keyed.Runner[int, pmsg, pstate]
	obs     *observer
	keys    [propKeys]*keyModel
	elapsed time.Duration
	stopped bool
	atStop  int
	// inFlightAtStop counts the handlers that were blocked when the Stop began.
	inFlightAtStop int
	timers         []timerModel
	dropped        uint64
	coalesced      uint64
	refused        uint64
	childPanics    uint64
	asks           []*askRecord
	broken         []string
}

// askRecord is an Ask made by the sequence and what the model expects of it.
type askRecord struct {
	id      int
	key     int
	refused bool // the model's mailbox refused it
	late    bool // sent after the Stop
	cancel  context.CancelFunc
	mu      sync.Mutex
	done    bool
	value   any
	err     error
}

func (a *askRecord) finish(value any, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.done, a.value, a.err = true, value, err
}

func newReplay(t *testing.T) *replay {
	root := task.NewRoot(context.WithoutCancel(t.Context()), "prop", zerolog.Nop())
	obs := newObserver()
	p := &replay{
		t:    t,
		root: root,
		obs:  obs,
		runner: keyed.New[int, pmsg, pstate](root, "prop", obs,
			keyed.Options[pmsg]{
				MailboxSize: propSize, IdleAfter: propIdle, Class: classifyProp,
				CoalesceKey: func(m pmsg) string { return m.co },
			}),
	}
	for i := range p.keys {
		p.keys[i] = &keyModel{}
	}
	return p
}

func (p *replay) failf(format string, args ...any) {
	p.broken = append(p.broken, fmt.Sprintf(format, args...))
}

// drain is the model of handlers running items in order: each is handled, and a child task's result queues behind
// what already waits.
func (p *replay) drain(key int, items []qItem) {
	km := p.keys[key]
	for i := 0; i < len(items); i++ {
		item := items[i]
		km.expected = append(km.expected, item.id)
		switch item.spawn {
		case 1:
			items = append(items, qItem{id: item.id + childBase, bypass: true})
		case 2:
			p.childPanics++
			items = append(items, qItem{id: item.id + panicBase, bypass: true})
		}
	}
}

// deliver is the model of a message reaching a key. It reports whether the mailbox refused it.
func (p *replay) deliver(key int, item qItem) bool {
	km := p.keys[key]
	switch {
	case !km.blocked && km.armed:
		km.armed, km.blocked, km.inflight = false, true, item
	case !km.blocked:
		p.drain(key, []qItem{item})
	case item.bypass:
		km.queue = append(km.queue, item)
	case item.obs:
		if i := km.queuedWithCoalesceKey(item.co); i >= 0 {
			km.queue[i] = item // the latest replaces the queued one, in its place
			p.coalesced++
			return false
		}
		if km.depth() >= propSize {
			if !km.evictOldestObservation() {
				p.refused++
				return true
			}
			p.dropped++
		}
		km.queue = append(km.queue, item)
	default:
		if km.depth() >= propSize {
			if !km.evictOldestObservation() {
				p.refused++
				return true
			}
			p.dropped++
		}
		km.queue = append(km.queue, item)
	}
	return false
}

// releaseKey is the model of letting a blocked handler go: it finishes, then the queue behind it drains.
func (p *replay) releaseKey(key int) {
	km := p.keys[key]
	km.armed = false
	p.obs.release(key)
	if !km.blocked {
		return
	}
	items := append([]qItem{km.inflight}, km.queue...)
	km.blocked, km.inflight, km.queue = false, qItem{}, nil
	p.drain(key, items)
}

// passTime lets d pass and delivers the timers that fell due, in due order.
func (p *replay) passTime(d time.Duration) {
	time.Sleep(d)
	p.elapsed += d
	if p.stopped {
		return // the Stop cancelled every timer
	}
	var due []int
	for i, timer := range p.timers {
		if !timer.fired && !timer.cancelled && timer.due <= p.elapsed {
			due = append(due, i)
		}
	}
	slices.SortStableFunc(due, func(a, b int) int {
		if c := p.timers[a].due - p.timers[b].due; c != 0 {
			return int(c)
		}
		return int(p.timers[a].id) - int(p.timers[b].id)
	})
	for _, i := range due {
		p.timers[i].fired = true
		p.deliver(p.timers[i].key, qItem{id: p.timers[i].msg, bypass: true, timer: int(p.timers[i].id)})
	}
}

func (p *replay) settle() {
	p.passTime(propTick)
	// A message delivered by the last timer of the tick is still in the handler's microsecond sleep; let it end.
	// The model's clock does not count these few microseconds: timers are set in whole milliseconds.
	time.Sleep(propSettle)
	synctest.Wait()
}

func (p *replay) stop() {
	if p.stopped {
		return
	}
	// The Stop begins with the holds in place, so handlers are blocked and messages queue behind them. They are
	// released only once the Stop is under way: each in-flight message finishes, nothing queued is handled.
	p.atStop = p.obs.total()
	done := make(chan error, 1)
	go func() {
		ctx, cancel := task.Detached(p.t.Context(), time.Minute)
		defer cancel()
		done <- p.root.Stop(ctx)
	}()
	synctest.Wait() // the Stop has cancelled the runner and waits for the handlers in flight
	p.stopped = true
	for key, km := range p.keys {
		if km.blocked {
			p.inFlightAtStop++
			km.expected = append(km.expected, km.inflight.id)
		}
		km.armed, km.blocked, km.inflight, km.queue = false, false, qItem{}, nil
		p.obs.release(key)
	}
	if err := <-done; err != nil {
		p.failf("Stop: %v", err)
	}
	synctest.Wait()
}

// ask is an Ask in a goroutine of its own, since it waits behind a blocked handler. The model delivers it as a
// command; whether it was refused is known at once, and the rest is checked at the end.
func (p *replay) ask(i int, o op) {
	rec := &askRecord{id: i, key: o.key, late: p.stopped}
	p.asks = append(p.asks, rec)
	if !p.stopped {
		rec.refused = p.deliver(o.key, qItem{id: i})
	}
	ctx, cancel := context.WithCancel(p.t.Context())
	rec.cancel = cancel
	go func() {
		value, err := p.runner.Ask(ctx, o.key, pmsg{id: i})
		rec.finish(value, err)
	}()
}

func (p *replay) send(i int, o op) {
	item := qItem{id: i, obs: o.kind == opObserve, co: coalesceKey(o.at)}
	if !item.obs {
		item.co = ""
	}
	if o.kind == opSendChild {
		item.spawn = 1
	}
	if o.kind == opSendPanic {
		item.spawn = 2
	}
	if p.stopped {
		if err := p.runner.Send(o.key, pmsg{id: i}); !errors.Is(err, keyed.ErrStopped) {
			p.failf("op %d: Send after Stop = %v, want ErrStopped", i, err)
		}
		return
	}
	refused := p.deliver(o.key, item)
	err := p.runner.Send(o.key, pmsg{id: i, obs: item.obs, co: item.co, spawn: item.spawn})
	switch {
	case refused && !errors.Is(err, keyed.ErrMailboxFull):
		p.failf("op %d: Send = %v, want ErrMailboxFull", i, err)
	case !refused && err != nil:
		p.failf("op %d: Send = %v, want it accepted", i, err)
	}
}

func (p *replay) after(i int, o op) {
	delay := time.Duration(o.at) * time.Millisecond
	id := p.runner.After(o.key, delay, pmsg{id: timerBase + i})
	switch {
	case id == 0 && !p.stopped:
		p.failf("op %d: After returned no timer", i)
	case id != 0:
		p.timers = append(p.timers, timerModel{id: id, key: o.key, msg: timerBase + i, due: p.elapsed + delay})
	}
}

// cancelCandidates lists the timers a cancel can still change: first those that fired and whose message waits
// behind a blocked handler (the recall path), then those not yet due. A blind pick over every timer would rarely
// land on the first kind.
func (p *replay) cancelCandidates() []int {
	var queued, pending []int
	for i, timer := range p.timers {
		switch {
		case timer.cancelled:
		case !timer.fired:
			pending = append(pending, i)
		case slices.ContainsFunc(p.keys[timer.key].queue, func(item qItem) bool { return item.timer == int(timer.id) }):
			queued = append(queued, i)
		}
	}
	return append(queued, pending...)
}

func (p *replay) cancel(o op) {
	candidates := p.cancelCandidates()
	if len(candidates) == 0 {
		return
	}
	timer := &p.timers[candidates[o.at%len(candidates)]]
	p.runner.CancelTimer(timer.id)
	if !timer.fired {
		timer.cancelled = true
		return
	}
	// A fired timer whose message still waits behind a blocked handler has it taken back; one being handled does not.
	km := p.keys[timer.key]
	km.queue = slices.DeleteFunc(km.queue, func(item qItem) bool { return item.timer == int(timer.id) })
}

func (p *replay) step(i int, o op) {
	switch o.kind {
	case opAsk:
		p.ask(i, o)
	case opSend, opObserve, opSendChild, opSendPanic:
		p.send(i, o)
	case opAfter:
		p.after(i, o)
	case opCancel:
		p.cancel(o)
	case opAdvance:
		p.passTime(time.Duration(o.at) * time.Millisecond)
	case opHold:
		if km := p.keys[o.key]; !p.stopped && !km.blocked && !km.armed {
			km.armed = true
			p.obs.arm(o.key)
		}
	case opRelease:
		p.releaseKey(o.key)
	case opStop:
		p.stop()
	}
	p.settle()
}

// check compares what the handlers saw with what the model expects.
func (p *replay) check() {
	p.obs.mu.Lock()
	defer p.obs.mu.Unlock()
	p.broken = append(p.broken, p.obs.broken...)
	// Once the Stop has begun only the messages in flight finish; everything queued behind them is dropped.
	if after := p.obs.count - p.atStop; after != p.inFlightAtStop {
		p.failf("%d messages were handled after the Stop began, want the %d in flight", after, p.inFlightAtStop)
	}
	for key := range propKeys {
		if got, want := p.obs.handled[key], p.keys[key].expected; !slices.Equal(got, want) {
			p.failf("key %d: handled %v, want %v", key, got, want)
		}
	}
	info := p.runner.Snapshot()
	if info.Dropped != p.dropped || info.Refused != p.refused || info.ChildPanics != p.childPanics ||
		info.Coalesced != p.coalesced {
		p.failf("counters: dropped %d refused %d child panics %d coalesced %d; want %d %d %d %d",
			info.Dropped, info.Refused, info.ChildPanics, info.Coalesced,
			p.dropped, p.refused, p.childPanics, p.coalesced)
	}
	p.checkAsks()
}

// checkAsks expects each Ask to have ended as the model says: refused at once, answered with its own id when it
// was handled, refused after the Stop, or told the outcome is unknown when the Stop dropped it from the queue.
func (p *replay) checkAsks() {
	for _, ask := range p.asks {
		ask.mu.Lock()
		done, value, err := ask.done, ask.value, ask.err
		ask.mu.Unlock()
		handled := slices.Contains(p.keys[ask.key].expected, ask.id)
		switch {
		case !done:
			p.failf("ask %d never returned", ask.id)
		case ask.late && !errors.Is(err, keyed.ErrStopped):
			p.failf("ask %d after the Stop = %v, want ErrStopped", ask.id, err)
		case ask.late:
		case ask.refused && !errors.Is(err, keyed.ErrMailboxFull):
			p.failf("ask %d = %v, want ErrMailboxFull", ask.id, err)
		case ask.refused:
		case handled && (err != nil || value != ask.id):
			p.failf("ask %d = %v, %v; want its own id", ask.id, value, err)
		case !handled && (!errors.Is(err, keyed.ErrOutcomeUnknown) || !errors.Is(err, keyed.ErrStopped)):
			p.failf("ask %d = %v, %v; want unknown, wrapping ErrStopped", ask.id, value, err)
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
			p.passTime(time.Second) // every pending timer falls due
			synctest.Wait()
		}
		p.stop()
		time.Sleep(time.Hour)
		synctest.Wait()
		p.check()
		for _, ask := range p.asks {
			ask.cancel() // an Ask that never returned must not hold the bubble
		}
		synctest.Wait()
		broken = p.broken
	})
	return broken
}

func TestRunnerHoldsItsInvariantsOverRandomSequences(t *testing.T) {
	t.Parallel()
	proptest.CheckWhy(t, proptest.Config{Seeds: 300}, genOps,
		func(ops []op) []string { return violations(t, ops) }, simplifyOp)
}
