package keyed_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

// msg is the test message: an id, and an optional hook the handler runs.
type msg struct {
	id    int
	block <-chan struct{} // the handler waits for it to close
	panic bool
	run   func(ctx context.Context, fx *keyed.Effects[string, msg])
}

// recorder is a handler that records what it saw.
type recorder struct {
	mu       sync.Mutex
	handled  map[string][]int
	inFlight map[string]int
	overlap  atomic.Bool
	entered  chan string // a key, each time a handler starts, when set
}

func newRecorder() *recorder {
	return &recorder{handled: map[string][]int{}, inFlight: map[string]int{}}
}

func (r *recorder) Activate(context.Context, string) struct{} { return struct{}{} }

func (r *recorder) Handle(ctx context.Context, key string, _ *struct{}, m msg, fx *keyed.Effects[string, msg]) {
	r.mu.Lock()
	r.inFlight[key]++
	if r.inFlight[key] > 1 {
		r.overlap.Store(true)
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.inFlight[key]--
		r.mu.Unlock()
	}()
	if r.entered != nil {
		r.entered <- key
	}
	if m.block != nil {
		<-m.block
	}
	if m.panic {
		panic("boom")
	}
	if m.run != nil {
		m.run(ctx, fx)
	}
	r.mu.Lock()
	r.handled[key] = append(r.handled[key], m.id)
	r.mu.Unlock()
}

func (r *recorder) got(key string) []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.handled[key])
}

// syncBuffer is a log sink safe for concurrent tasks.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newRoot(t testing.TB) *task.Group {
	t.Helper()
	g := task.NewRoot(context.WithoutCancel(t.Context()), "test", zerolog.Nop())
	t.Cleanup(func() {
		ctx, cancel := task.Detached(t.Context(), time.Minute)
		defer cancel()
		if err := g.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return g
}

func stop(t testing.TB, g *task.Group) {
	t.Helper()
	ctx, cancel := task.Detached(t.Context(), time.Minute)
	defer cancel()
	if err := g.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func mustSend(t testing.TB, r *keyed.Runner[string, msg, struct{}], key string, m msg) {
	t.Helper()
	if err := r.Send(key, m); err != nil {
		t.Fatalf("Send(%q, %d) = %v", key, m.id, err)
	}
}

func TestMessagesToAKeyAreHandledInOrder(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](newRoot(t), "orders", rec, keyed.Options[msg]{MailboxSize: 128})
		var want []int
		for i := range 100 {
			mustSend(t, r, "a", msg{id: i})
			want = append(want, i)
		}
		synctest.Wait()
		if got := rec.got("a"); !slices.Equal(got, want) {
			t.Fatalf("handled %v, want %v", got, want)
		}
	})
}

func TestKeysRunInParallel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](newRoot(t), "parallel", rec, keyed.Options[msg]{})
		release := make(chan struct{})
		mustSend(t, r, "slow", msg{id: 1, block: release})
		synctest.Wait()

		mustSend(t, r, "fast", msg{id: 2})
		synctest.Wait()

		if got := rec.got("fast"); !slices.Equal(got, []int{2}) {
			t.Fatalf("fast handled %v while slow was blocked, want [2]", got)
		}
		if got := rec.got("slow"); len(got) != 0 {
			t.Fatalf("slow handled %v before its release", got)
		}
		close(release)
		synctest.Wait()
		if got := rec.got("slow"); !slices.Equal(got, []int{1}) {
			t.Fatalf("slow handled %v after its release, want [1]", got)
		}
	})
}

func TestOneHandlerAtATimePerKeyUnderConcurrentSends(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](newRoot(t), "serial", rec, keyed.Options[msg]{MailboxSize: 1_000})
		var senders sync.WaitGroup
		for s := range 20 {
			senders.Go(func() {
				for i := range 20 {
					_ = r.Send("a", msg{id: s*100 + i})
				}
			})
		}
		senders.Wait()
		synctest.Wait()

		if rec.overlap.Load() {
			t.Fatal("two handlers for one key ran at the same time")
		}
		if got := len(rec.got("a")); got != 400 {
			t.Fatalf("handled %d messages, want 400", got)
		}
		// Each sender's own messages keep their order.
		bySender := map[int][]int{}
		for _, id := range rec.got("a") {
			bySender[id/100] = append(bySender[id/100], id%100)
		}
		for s, ids := range bySender {
			if !slices.IsSorted(ids) {
				t.Fatalf("sender %d's messages were handled out of order: %v", s, ids)
			}
		}
	})
}

func TestAKeyExitsWhenIdleAndRestartsOnSend(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		g := newRoot(t)
		r := keyed.New[string, msg, struct{}](g, "idle", rec, keyed.Options[msg]{IdleAfter: time.Second})
		mustSend(t, r, "a", msg{id: 1})
		synctest.Wait()
		if info := r.Snapshot(); len(info.Keys) != 1 || !info.Keys[0].Running {
			t.Fatalf("snapshot = %+v, want key a running", info)
		}

		time.Sleep(2 * time.Second)
		synctest.Wait()

		if info := r.Snapshot(); len(info.Keys) != 0 {
			t.Fatalf("snapshot = %+v, want no keys after the idle exit", info)
		}
		for _, running := range g.Snapshot() {
			if strings.HasSuffix(running.Path, ":a") {
				t.Fatalf("the key's task is still running: %v", running.Path)
			}
		}
		mustSend(t, r, "a", msg{id: 2})
		synctest.Wait()
		if got := rec.got("a"); !slices.Equal(got, []int{1, 2}) {
			t.Fatalf("handled %v, want [1 2]", got)
		}
	})
}

func TestSendAfterStopIsRefusedAndNothingIsLeftRunning(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		g := newRoot(t)
		r := keyed.New[string, msg, struct{}](g, "stop", rec, keyed.Options[msg]{})
		mustSend(t, r, "a", msg{id: 1})
		synctest.Wait()

		stop(t, g)

		if err := r.Send("a", msg{id: 2}); !errors.Is(err, keyed.ErrStopped) {
			t.Fatalf("Send after Stop = %v, want ErrStopped", err)
		}
		if left := g.Snapshot(); len(left) != 0 {
			t.Fatalf("tasks left after Stop: %v", left)
		}
		if got := rec.got("a"); !slices.Equal(got, []int{1}) {
			t.Fatalf("handled %v, want only the message sent before Stop", got)
		}
		if got := r.Snapshot().State; got != "stopped" {
			t.Fatalf("state = %q, want stopped", got)
		}
	})
}

func TestStopWaitsForTheMessageInFlightAndDropsTheQueue(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		g := newRoot(t)
		r := keyed.New[string, msg, struct{}](g, "inflight", rec, keyed.Options[msg]{})
		release := make(chan struct{})
		mustSend(t, r, "a", msg{id: 1, block: release})
		mustSend(t, r, "a", msg{id: 2})
		synctest.Wait()

		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			stop(t, g)
		}()
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("Stop returned while a handler was running")
		default:
		}
		close(release)
		<-stopped

		if got := rec.got("a"); !slices.Equal(got, []int{1}) {
			t.Fatalf("handled %v, want only the message in flight", got)
		}
	})
}

func TestAHandlerPanicIsRecoveredAndTheNextMessageHandled(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		logs := &syncBuffer{}
		root := task.NewRoot(context.WithoutCancel(t.Context()), "test", zerolog.New(logs))
		t.Cleanup(func() { stop(t, root) })
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](root, "panics", rec, keyed.Options[msg]{Logger: zerolog.New(logs)})
		mustSend(t, r, "a", msg{id: 1, panic: true})
		mustSend(t, r, "a", msg{id: 2})
		synctest.Wait()

		if got := rec.got("a"); !slices.Equal(got, []int{2}) {
			t.Fatalf("handled %v, want the message after the panic", got)
		}
		if !strings.Contains(logs.String(), `"key":"a"`) || !strings.Contains(logs.String(), "panicked") {
			t.Fatalf("the panic was not logged with its key: %s", logs.String())
		}
		select {
		case <-root.Failed():
			t.Fatal("a handler panic failed the group")
		default:
		}
		if got := r.Snapshot().Panics; got != 1 {
			t.Fatalf("panics = %d, want 1", got)
		}
	})
}

func TestSnapshotShowsDepthAndAgeOfTheOldestMessage(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](newRoot(t), "snapshot", rec, keyed.Options[msg]{})
		release := make(chan struct{})
		mustSend(t, r, "a", msg{id: 1, block: release})
		synctest.Wait()
		mustSend(t, r, "a", msg{id: 2})
		time.Sleep(3 * time.Second)
		mustSend(t, r, "a", msg{id: 3})

		info := r.Snapshot()

		if len(info.Keys) != 1 {
			t.Fatalf("snapshot = %+v, want one key", info)
		}
		key := info.Keys[0]
		if key.Key != "a" || key.Depth != 2 || key.OldestAge != 3*time.Second || !key.Running {
			t.Fatalf("key = %+v, want a: depth 2, oldest 3s, running", key)
		}
		close(release)
		synctest.Wait()
		if after := r.Snapshot().Keys; len(after) != 1 || after[0].Depth != 0 || after[0].OldestAge != 0 {
			t.Fatalf("after the drain: %+v, want depth 0 and age 0", after)
		}
	})
}

func TestACommandIsRefusedWhenTheMailboxIsFull(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](newRoot(t), "full", rec, keyed.Options[msg]{MailboxSize: 2})
		release := make(chan struct{})
		mustSend(t, r, "a", msg{id: 1, block: release}) // in flight, so not queued
		synctest.Wait()
		mustSend(t, r, "a", msg{id: 2})
		mustSend(t, r, "a", msg{id: 3})

		if err := r.Send("a", msg{id: 4}); !errors.Is(err, keyed.ErrMailboxFull) {
			t.Fatalf("Send into a full mailbox = %v, want ErrMailboxFull", err)
		}
		mustSend(t, r, "b", msg{id: 5}) // another key has room
		close(release)
		synctest.Wait()
		if got := rec.got("a"); !slices.Equal(got, []int{1, 2, 3}) {
			t.Fatalf("handled %v, want [1 2 3]", got)
		}
	})
}

func TestEffectsGoReportsBackAsAMessageAndSendReachesOtherKeys(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](newRoot(t), "effects", rec, keyed.Options[msg]{})
		mustSend(t, r, "a", msg{id: 1, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			if err := fx.Go("fetch", func(context.Context) msg { return msg{id: 10} }, nil); err != nil {
				t.Error(err)
			}
			if err := fx.Send("b", msg{id: 20}); err != nil {
				t.Error(err)
			}
		}})
		synctest.Wait()

		if got := rec.got("a"); !slices.Equal(got, []int{1, 10}) {
			t.Fatalf("a handled %v, want its own message and its child's result", got)
		}
		if got := rec.got("b"); !slices.Equal(got, []int{20}) {
			t.Fatalf("b handled %v, want the message a sent", got)
		}
	})
}

func TestAChildResultIsDeliveredEvenWhenTheMailboxIsFull(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](newRoot(t), "results", rec, keyed.Options[msg]{MailboxSize: 1})
		finish := make(chan struct{})
		release := make(chan struct{})
		mustSend(t, r, "a", msg{id: 1, run: func(_ context.Context, fx *keyed.Effects[string, msg]) {
			_ = fx.Go("io", func(context.Context) msg {
				<-finish
				return msg{id: 99}
			}, nil)
			<-release
		}})
		synctest.Wait()
		mustSend(t, r, "a", msg{id: 2}) // fills the mailbox
		close(finish)
		synctest.Wait()
		close(release)
		synctest.Wait()

		if got := rec.got("a"); !slices.Equal(got, []int{1, 2, 99}) {
			t.Fatalf("handled %v, want the result queued past the bound: [1 2 99]", got)
		}
	})
}

func TestASingletonStaysUpThroughIdleTime(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		got := make(chan int, 4)
		h := keyed.HandlerFunc[struct{}, int](
			func(_ context.Context, _ struct{}, m int, _ *keyed.Effects[struct{}, int]) {
				got <- m
			},
		)
		g := newRoot(t)
		r := keyed.NewSingleton[int, struct{}](g, "single", h, keyed.Options[int]{IdleAfter: time.Second})
		synctest.Wait()

		time.Sleep(time.Hour)
		synctest.Wait()
		if err := r.Send(struct{}{}, 7); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()

		if v := <-got; v != 7 {
			t.Fatalf("handled %d, want 7", v)
		}
		if info := r.Snapshot(); len(info.Keys) != 1 || !info.Keys[0].Running {
			t.Fatalf("snapshot = %+v, want the one key still running", info)
		}
	})
}
