package manager

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/proptest"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type propOpKind uint8

const (
	propSubmit propOpKind = iota
	propAdmitted
	propReply
	propDelete
	propTags
	propWait
	propRestart
)

type propOp struct {
	kind propOpKind
	key  int
	v    int
}

func propKeys() []string { return []string{"a", "b", "c"} }

func (o propOp) String() string {
	key := propKeys()[o.key]
	switch o.kind {
	case propSubmit:
		return fmt.Sprintf("submit(%s,nzb=%v)", key, o.v == 1)
	case propAdmitted:
		return "admitted(" + key + ")"
	case propReply:
		return fmt.Sprintf("reply(%s,%d)", key, o.v)
	case propDelete:
		return "delete(" + key + ")"
	case propTags:
		return "tags(" + key + ")"
	case propWait:
		return fmt.Sprintf("wait(%d)", o.v)
	case propRestart:
		return "restart"
	}
	return "?"
}

const (
	propReplyVariants = 5
	propOpsPerRun     = 40
	propRetryDelay    = 10 * time.Second
)

func genPropOps(seed int64) []propOp {
	r := rand.New(rand.NewPCG(uint64(seed), 0))
	ops := make([]propOp, propOpsPerRun)
	for i := range ops {
		op := propOp{key: r.IntN(len(propKeys()))}
		switch n := r.IntN(20); {
		case n < 3:
			op.kind, op.v = propSubmit, r.IntN(2)
		case n < 4:
			op.kind = propAdmitted
		case n < 13:
			op.kind, op.v = propReply, r.IntN(propReplyVariants)
		case n < 15:
			op.kind = propDelete
		case n < 16:
			op.kind = propTags
		case n < 19:
			op.kind, op.v = propWait, r.IntN(2)
		default:
			op.kind = propRestart
		}
		ops[i] = op
	}
	return ops
}

func simplifyPropOp(o propOp) []propOp {
	if o.v > 0 && (o.kind == propReply || o.kind == propWait) {
		return []propOp{{kind: o.kind, key: o.key}}
	}
	return nil
}

type propChild struct {
	key     string
	kind    grab.ChildKind
	reply   chan entryMsg
	replied bool
	done    bool
}

type propLive struct {
	key  string
	kind grab.ChildKind
}

type propRun struct {
	t     *testing.T
	store *storage.Storage
	h     *actorHarness

	mu       sync.Mutex
	children []*propChild
	live     map[propLive]int
	broken   []string

	gone   map[string]bool
	phases map[grab.Phase]bool
}

func (r *propRun) violatef(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.broken = append(r.broken, fmt.Sprintf(format, args...))
}

func (r *propRun) child(call entryChildCall) func(ctx context.Context) entryMsg {
	c := &propChild{key: call.grab.State.Key, kind: call.kind, reply: make(chan entryMsg, 1)}
	return func(ctx context.Context) entryMsg {
		k := propLive{key: c.key, kind: c.kind}
		r.mu.Lock()
		r.live[k]++
		if r.live[k] > 1 {
			r.broken = append(r.broken, fmt.Sprintf("%s has two live %s children", c.key, childKind(c.kind)))
		}
		r.children = append(r.children, c)
		r.mu.Unlock()
		defer func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.live[k]--
			c.done = true
		}()
		select {
		case m := <-c.reply:
			return m
		case <-ctx.Done():
			return entryMsg{}
		}
	}
}

func (r *propRun) harness() {
	r.h = newActorHarness(r.t, r.store)
	r.h.a.child = r.child
	r.h.a.retryDelay = func(int) time.Duration { return propRetryDelay }
}

func propTorrent(key string, downloaded bool) *debridTypes.Torrent {
	status := debridTypes.TorrentStatusDownloading
	if downloaded {
		status = debridTypes.TorrentStatusDownloaded
	}
	return &debridTypes.Torrent{ID: "t-" + key, InfoHash: key, Name: key, Debrid: "realdebrid", Status: status}
}

func propPlaced(kind grab.EventKind, outcome grab.Outcome, key string, downloaded, linked bool) entryMsg {
	m := eventMsg(grab.Event{
		Kind: kind, Outcome: outcome, Provider: "realdebrid", TorrentID: "t-" + key,
		Downloaded: downloaded, AllLinked: linked,
	})
	m.torrent = propTorrent(key, downloaded)
	return m
}

func childReply(kind grab.ChildKind, key string, v int) entryMsg {
	switch kind {
	case grab.ChildSubmit:
		return [...]entryMsg{
			propPlaced(grab.EvSubmitResult, grab.OutcomePlacement, key, false, false),
			propPlaced(grab.EvSubmitResult, grab.OutcomePlacement, key, true, true),
			submitResult(nil, nil, &throttledError{providers: []string{"realdebrid"}}),
			submitResult(nil, nil, errors.New("refused")),
			{},
		}[v]
	case grab.ChildLookup:
		return [...]entryMsg{
			propPlaced(grab.EvLookup, grab.OutcomeFound, key, false, false),
			eventMsg(grab.Event{Kind: grab.EvLookup, Outcome: grab.OutcomeAbsent}),
			{}, {}, {},
		}[v]
	case grab.ChildPoll:
		return [...]entryMsg{
			propPlaced(grab.EvProviderObserved, 0, key, false, false),
			propPlaced(grab.EvProviderObserved, 0, key, true, true),
			propPlaced(grab.EvProviderObserved, 0, key, true, false),
			eventMsg(grab.Event{Kind: grab.EvProviderObserved, Failed: true, Fail: grab.FailEntry}),
			{},
		}[v]
	case grab.ChildLinks:
		return [...]entryMsg{
			eventMsg(grab.Event{Kind: grab.EvLinksReady}),
			eventMsg(grab.Event{Kind: grab.EvLinkWaitExpired, Fail: grab.FailEntry}),
			{}, {}, {},
		}[v]
	case grab.ChildAction:
		return [...]entryMsg{
			eventMsg(grab.Event{Kind: grab.EvActionDone}),
			eventMsg(grab.Event{Kind: grab.EvActionFailed, Fail: grab.FailEntry}),
			{exitErr: ErrMountStopping},
			{exitErr: context.Canceled},
			{},
		}[v]
	case grab.ChildCleanup:
		if v == propReplyVariants-1 {
			return entryMsg{}
		}
		return eventMsg(grab.Event{Kind: grab.EvCleanupDone})
	}
	return entryMsg{}
}

func (r *propRun) oldest(key string) *propChild {
	r.mu.Lock()
	defer r.mu.Unlock()
	var pick *propChild
	for _, c := range r.children {
		if c.key != key || c.done || c.replied {
			continue
		}
		if pick == nil || c.kind < pick.kind {
			pick = c
		}
	}
	if pick != nil {
		pick.replied = true
	}
	return pick
}

func (r *propRun) ask(key string, m entryMsg) error {
	v, err := r.h.a.runner.Ask(r.t.Context(), key, m)
	if err == nil && v != nil {
		err, _ = v.(error)
	}
	return err
}

func (r *propRun) diskGrab(key string) (grab.Phase, bool) {
	e, err := r.store.GetQueuedGrab(key)
	if errors.Is(err, appendstore.ErrKeyNotFound) {
		return grab.PhaseAbsent, false
	}
	if err != nil {
		r.t.Fatal(err)
	}
	g, err := grab.Load(e, nil, false)
	if err != nil {
		r.t.Fatal(err)
	}
	return g.State.Phase, g.State.NZB
}

func (r *propRun) memPhase(key string) grab.Phase {
	if v, ok := r.h.grabs.get(key); ok {
		return v.State.Phase
	}
	return grab.PhaseAbsent
}

func (r *propRun) do(i int, op propOp) {
	key := propKeys()[op.key]
	switch op.kind {
	case propSubmit:
		e := testEntry(key)
		if op.v == 1 {
			e.Protocol = config.ProtocolNZB
		}
		msg := eventMsg(grab.Event{Kind: grab.EvSubmit})
		msg.entry = e
		if err := r.ask(key, msg); err == nil {
			r.gone[key] = false
		}
	case propAdmitted:
		_ = r.ask(key, eventMsg(grab.Event{Kind: grab.EvAdmitted}))
	case propReply:
		if c := r.oldest(key); c != nil {
			c.reply <- childReply(c.kind, key, op.v)
		}
	case propDelete:
		_ = r.ask(key, eventMsg(grab.Event{Kind: grab.EvDelete, Cleanup: grab.Cleanup{DeleteFiles: op.v == 1}}))
	case propTags:
		msg := eventMsg(grab.Event{Kind: grab.EvTagsChanged})
		msg.tags = []string{strconv.Itoa(i)}
		_ = r.ask(key, msg)
	case propWait:
		time.Sleep([...]time.Duration{testPollEvery + time.Second, time.Minute + propRetryDelay + time.Second}[op.v])
	case propRestart:
		r.restart(i)
		return
	}
	synctest.Wait()
	r.check(i, op)
}

func (r *propRun) restart(i int) {
	before := make(map[string]grab.Phase, len(propKeys()))
	nzb := make(map[string]bool, len(propKeys()))
	for _, key := range propKeys() {
		before[key], nzb[key] = r.diskGrab(key)
	}
	r.h.stop()
	for _, key := range propKeys() {
		if got, _ := r.diskGrab(key); got != before[key] {
			r.violatef("step %d: the stop moved %s on disk from %v to %v", i, key, before[key], got)
		}
	}
	r.harness()
	recoverAll(r.h)
	synctest.Wait()
	for _, key := range propKeys() {
		got := r.memPhase(key)
		if !slices.Contains(recoverable(before[key], nzb[key]), got) {
			r.violatef("step %d: %s recovered from %v to %v", i, key, before[key], got)
		}
		if kind, ok := resumedBy(got); ok && !r.running(key, kind) {
			r.violatef("step %d: %s recovered in %v with no %s child", i, key, got, childKind(kind))
		}
	}
	r.check(i, propOp{kind: propRestart})
}

func recoverable(p grab.Phase, nzb bool) []grab.Phase {
	switch p {
	case grab.PhaseQueued, grab.PhaseThrottled:
		return []grab.Phase{grab.PhaseQueued, grab.PhaseSubmitting}
	case grab.PhaseSubmitting:
		if nzb {
			return []grab.Phase{grab.PhaseQueued, grab.PhaseSubmitting}
		}
	case grab.PhaseAbsent, grab.PhaseProvider, grab.PhaseAwaitingLinks, grab.PhaseActing, grab.PhaseComplete,
		grab.PhaseFailed, grab.PhaseDeleting:
	}
	return []grab.Phase{p}
}

func resumedBy(p grab.Phase) (grab.ChildKind, bool) {
	switch p {
	case grab.PhaseProvider:
		return grab.ChildPoll, true
	case grab.PhaseAwaitingLinks:
		return grab.ChildLinks, true
	case grab.PhaseActing:
		return grab.ChildAction, true
	case grab.PhaseDeleting:
		return grab.ChildCleanup, true
	case grab.PhaseAbsent, grab.PhaseQueued, grab.PhaseThrottled, grab.PhaseSubmitting, grab.PhaseComplete,
		grab.PhaseFailed:
	}
	return 0, false
}

func (r *propRun) running(key string, kind grab.ChildKind) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live[propLive{key: key, kind: kind}] > 0
}

func (r *propRun) check(i int, op propOp) {
	for _, key := range propKeys() {
		mem := r.memPhase(key)
		r.phases[mem] = true
		if v, ok := r.h.grabs.get(key); ok {
			if err := v.State.Validate(); err != nil {
				r.violatef("step %d %v: %s invalid: %v", i, op, key, err)
			}
		}
		disk, _ := r.diskGrab(key)
		if mem != disk {
			r.violatef("step %d %v: %s is %v in memory but %v on disk", i, op, key, mem, disk)
		}
		if r.gone[key] && disk != grab.PhaseAbsent {
			r.violatef("step %d %v: %s was written (%v) after its delete finished", i, op, key, disk)
		}
	}
}

func runProp(t *testing.T, ops []propOp, phases map[grab.Phase]bool) []string {
	t.Helper()
	var broken []string
	synctest.Test(t, func(t *testing.T) {
		r := &propRun{
			t: t, store: newTestStore(t), live: make(map[propLive]int), gone: make(map[string]bool),
			phases: phases,
		}
		r.harness()
		before := make(map[string]grab.Phase, len(propKeys()))
		for i, op := range ops {
			for _, key := range propKeys() {
				before[key], _ = r.diskGrab(key)
			}
			r.do(i, op)
			for _, key := range propKeys() {
				if after, _ := r.diskGrab(key); before[key] == grab.PhaseDeleting && after == grab.PhaseAbsent {
					r.gone[key] = true
				}
			}
		}
		r.h.stop()
		r.mu.Lock()
		broken = slices.Clone(r.broken)
		r.mu.Unlock()
	})
	return broken
}

func TestEntryActorProperty(t *testing.T) {
	t.Parallel()
	phases := make(map[grab.Phase]bool)
	proptest.CheckWhy(t, proptest.Config{Seeds: 20},
		genPropOps,
		func(ops []propOp) []string { return runProp(t, ops, phases) },
		simplifyPropOp,
	)
	for _, p := range grab.AllPhases() {
		if !phases[p] {
			t.Errorf("no run reached %v; the generator does not cover it", p)
		}
	}
}

func hammerReply(call entryChildCall) func(context.Context) entryMsg {
	key := call.grab.State.Key
	return func(context.Context) entryMsg {
		v := 0
		if call.kind == grab.ChildSubmit || call.kind == grab.ChildPoll {
			v = 1
		}
		return childReply(call.kind, key, v)
	}
}

func TestOneKeyHammer(t *testing.T) {
	t.Parallel()
	const key, workers = "a", 50
	store := newTestStore(t)
	h := newActorHarness(t, store)
	h.a.child = hammerReply
	m := h.libManager()
	ctx := t.Context()

	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			switch i % 4 {
			case 0:
				_ = m.submitEntry(ctx, testEntry(key), nil)
			case 1:
				_ = m.DeleteQueued(ctx, key, grab.Cleanup{DeleteFiles: i%8 == 1})
			case 2:
				_ = m.SetTags(ctx, key, []string{strconv.Itoa(i)})
			default:
				_ = m.writeLibrary(ctx, key, libraryOp{kind: libPut, entry: libEntry(key, strconv.Itoa(i))})
			}
		})
	}
	wg.Wait()
	h.stop()

	r := &propRun{t: t, store: store, h: h}
	mem := r.memPhase(key)
	disk, _ := r.diskGrab(key)
	if mem != disk {
		t.Fatalf("%s is %v in memory but %v on disk", key, mem, disk)
	}
	if v, ok := h.grabs.get(key); ok {
		if err := v.State.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}
