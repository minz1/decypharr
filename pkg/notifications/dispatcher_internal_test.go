package notifications

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

// outcome is what one send of a fake notifier does.
type outcome struct {
	err   error
	block bool // wait for the send's context to end, then return its error
	hang  chan struct{}
	panic bool
}

// sendRecord is one call of a fake notifier.
type sendRecord struct {
	id  string
	at  time.Time
	ctx context.Context
}

// fakeNotifier plays a script of outcomes, one per send; past the script, it delivers.
type fakeNotifier struct {
	name string

	mu       sync.Mutex
	script   []outcome
	calls    []sendRecord
	inFlight int
	overlap  bool
}

func newFake(name string, script ...outcome) *fakeNotifier {
	return &fakeNotifier{name: name, script: script}
}

func (f *fakeNotifier) Name() string { return f.name }

func (f *fakeNotifier) Send(ctx context.Context, event Event) error {
	f.mu.Lock()
	f.calls = append(f.calls, sendRecord{id: event.Message, at: time.Now(), ctx: ctx})
	f.inFlight++
	if f.inFlight > 1 {
		f.overlap = true
	}
	var o outcome
	if len(f.script) > 0 {
		o, f.script = f.script[0], f.script[1:]
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	switch {
	case o.panic:
		panic("notifier exploded")
	case o.block:
		<-ctx.Done()
		return ctx.Err()
	case o.hang != nil:
		select {
		case <-o.hang:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return o.err
}

func (f *fakeNotifier) sent() []sendRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// recordObserver records what the dispatcher reports.
type recordObserver struct {
	mu           sync.Mutex
	deliveredIDs map[string][]string
	dropList     map[string][]dropRecord
}

type dropRecord struct {
	id       string
	reason   dropReason
	attempts int
}

func newRecordObserver() *recordObserver {
	return &recordObserver{deliveredIDs: map[string][]string{}, dropList: map[string][]dropRecord{}}
}

func (o *recordObserver) delivered(notifier string, ev Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.deliveredIDs[notifier] = append(o.deliveredIDs[notifier], ev.Message)
}

func (o *recordObserver) dropped(notifier string, ev Event, reason dropReason, attempts int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.dropList[notifier] = append(o.dropList[notifier], dropRecord{id: ev.Message, reason: reason, attempts: attempts})
}

func (o *recordObserver) gotDelivered(notifier string) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.deliveredIDs[notifier])
}

func (o *recordObserver) gotDrops(notifier string) []dropRecord {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.dropList[notifier])
}

// logBuffer is a log sink safe for concurrent tasks.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) count(level, substr string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for line := range strings.SplitSeq(b.buf.String(), "\n") {
		if strings.Contains(line, `"level":"`+level+`"`) && strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

type rig struct {
	group  *task.Group
	runner *keyed.Runner[string, msg, notifierState]
	obs    *recordObserver
	logs   *logBuffer
}

func newRig(t *testing.T, notifiers ...Notifier) *rig {
	t.Helper()
	g := task.NewRoot(context.WithoutCancel(t.Context()), "test", zerolog.Nop())
	t.Cleanup(func() { stopGroup(t, g) })
	logs := &logBuffer{}
	obs := newRecordObserver()
	return &rig{
		group:  g,
		runner: newRunner(g, notifiers, zerolog.New(logs).Level(zerolog.TraceLevel), obs),
		obs:    obs,
		logs:   logs,
	}
}

func stopGroup(t *testing.T, g *task.Group) {
	t.Helper()
	ctx, cancel := task.Detached(t.Context(), time.Minute)
	defer cancel()
	if err := g.Stop(ctx); err != nil {
		t.Error(err)
	}
}

func (r *rig) notify(t *testing.T, notifier string, id int) {
	t.Helper()
	ev := Event{Type: config.EventDownloadComplete, Status: "success", Message: strconv.Itoa(id)}
	if err := r.runner.Send(notifier, msg{kind: msgEnqueue, ev: ev}); err != nil {
		t.Fatalf("Send(%s, %d) = %v", notifier, id, err)
	}
}

func offsets(start time.Time, calls []sendRecord) []time.Duration {
	out := make([]time.Duration, len(calls))
	for i, c := range calls {
		out[i] = c.at.Sub(start)
	}
	return out
}

func ids(n int, from int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, strconv.Itoa(from+i))
	}
	return out
}

var errNetwork = errors.New("dial tcp: connection refused")

func status(code int, retryAfter time.Duration) error {
	return &StatusError{Code: code, Status: strconv.Itoa(code), RetryAfter: retryAfter}
}

func TestRetriesThenDelivers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFake("hook", outcome{err: status(http.StatusServiceUnavailable, 0)}, outcome{err: errNetwork})
		r := newRig(t, f)
		start := time.Now()
		r.notify(t, "hook", 1)
		time.Sleep(time.Minute)
		synctest.Wait()
		if got, want := offsets(
			start,
			f.sent(),
		), []time.Duration{
			0,
			time.Second,
			6 * time.Second,
		}; !slices.Equal(
			got,
			want,
		) {
			t.Fatalf("sends at %v, want %v", got, want)
		}
		if got := r.obs.gotDelivered("hook"); !slices.Equal(got, []string{"1"}) {
			t.Fatalf("delivered %v, want [1]", got)
		}
		if drops := r.obs.gotDrops("hook"); len(drops) != 0 {
			t.Fatalf("drops %v, want none", drops)
		}
	})
}

func TestRejectedSentOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFake("hook", outcome{err: status(http.StatusBadRequest, 0)})
		r := newRig(t, f)
		r.notify(t, "hook", 1)
		time.Sleep(time.Minute)
		synctest.Wait()
		if n := len(f.sent()); n != 1 {
			t.Fatalf("sends = %d, want 1", n)
		}
		want := []dropRecord{{id: "1", reason: dropRejected, attempts: 1}}
		if got := r.obs.gotDrops("hook"); !slices.Equal(got, want) {
			t.Fatalf("drops %v, want %v", got, want)
		}
		if n := r.logs.count("warn", "Notification dropped"); n != 1 {
			t.Fatalf("drop WARNs = %d, want 1", n)
		}
	})
}

func TestDroppedAfterFourSends(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fail := outcome{err: status(http.StatusServiceUnavailable, 0)}
		f := newFake("hook", fail, fail, fail, fail, fail)
		r := newRig(t, f)
		start := time.Now()
		r.notify(t, "hook", 1)
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		want := []time.Duration{0, time.Second, 6 * time.Second, 36 * time.Second}
		if got := offsets(start, f.sent()); !slices.Equal(got, want) {
			t.Fatalf("sends at %v, want %v", got, want)
		}
		wantDrops := []dropRecord{{id: "1", reason: dropFailed, attempts: 4}}
		if got := r.obs.gotDrops("hook"); !slices.Equal(got, wantDrops) {
			t.Fatalf("drops %v, want %v", got, wantDrops)
		}
		if n := r.logs.count("warn", "Notification dropped"); n != 1 {
			t.Fatalf("drop WARNs = %d, want 1", n)
		}
	})
}

func TestRetryAfterHonoured(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		retryAfter time.Duration
		want       time.Duration
	}{
		{"12s", 12 * time.Second, 12 * time.Second},
		{"clamped", 120 * time.Second, 30 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newFake("hook", outcome{err: status(http.StatusTooManyRequests, tt.retryAfter)})
				r := newRig(t, f)
				start := time.Now()
				r.notify(t, "hook", 1)
				time.Sleep(time.Minute)
				synctest.Wait()
				if got, want := offsets(start, f.sent()), []time.Duration{0, tt.want}; !slices.Equal(got, want) {
					t.Fatalf("sends at %v, want %v", got, want)
				}
			})
		})
	}
}

func TestSendTimeoutRetries(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFake("hook", outcome{block: true})
		r := newRig(t, f)
		start := time.Now()
		r.notify(t, "hook", 1)
		time.Sleep(time.Minute)
		synctest.Wait()
		calls := f.sent()
		if got, want := offsets(start, calls), []time.Duration{0, 11 * time.Second}; !slices.Equal(got, want) {
			t.Fatalf("sends at %v, want %v", got, want)
		}
		if !errors.Is(calls[0].ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("first send ctx = %v, want deadline exceeded", calls[0].ctx.Err())
		}
		if got := r.obs.gotDelivered("hook"); !slices.Equal(got, []string{"1"}) {
			t.Fatalf("delivered %v, want [1]", got)
		}
	})
}

func TestNotifiersIndependent(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		slow := newFake("slow", outcome{err: status(http.StatusServiceUnavailable, 0)})
		fast := newFake("fast")
		r := newRig(t, slow, fast)
		r.notify(t, "slow", 1)
		synctest.Wait()
		for i := range 3 {
			r.notify(t, "fast", i)
		}
		synctest.Wait()
		if got := r.obs.gotDelivered("fast"); !slices.Equal(got, ids(3, 0)) {
			t.Fatalf("fast delivered %v while slow waits, want %v", got, ids(3, 0))
		}
		if got := r.obs.gotDelivered("slow"); len(got) != 0 {
			t.Fatalf("slow delivered %v before its retry", got)
		}
		if got := r.obs.gotDrops("slow"); len(got) != 0 {
			t.Fatalf("slow dropped %v while waiting to retry", got)
		}
	})
}

func TestFIFOPerNotifier(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFake("hook")
		r := newRig(t, f)
		for i := range 10 {
			r.notify(t, "hook", i)
		}
		synctest.Wait()
		if got := r.obs.gotDelivered("hook"); !slices.Equal(got, ids(10, 0)) {
			t.Fatalf("delivered %v, want %v", got, ids(10, 0))
		}
		f.mu.Lock()
		overlap := f.overlap
		f.mu.Unlock()
		if overlap {
			t.Fatal("two sends ran at once")
		}
	})
}

func TestOverflowDropsOldest(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		f := newFake("hook", outcome{hang: release})
		r := newRig(t, f)
		r.notify(t, "hook", 1)
		synctest.Wait()
		for i := 2; i <= 104; i++ {
			r.notify(t, "hook", i)
		}
		synctest.Wait()
		wantDrops := []dropRecord{
			{id: "2", reason: dropOverflow}, {id: "3", reason: dropOverflow}, {id: "4", reason: dropOverflow},
		}
		if got := r.obs.gotDrops("hook"); !slices.Equal(got, wantDrops) {
			t.Fatalf("drops %v, want %v", got, wantDrops)
		}
		if n := r.logs.count("warn", "Notification queue full"); n != 1 {
			t.Fatalf("overflow WARNs = %d, want 1", n)
		}
		close(release)
		synctest.Wait()
		want := append([]string{"1"}, ids(100, 5)...)
		if got := r.obs.gotDelivered("hook"); !slices.Equal(got, want) {
			t.Fatalf("delivered %d events %v, want %d in order", len(got), got, len(want))
		}
		if n := r.logs.count("info", "Notification queue drained"); n != 1 {
			t.Fatalf("drain INFOs = %d, want 1", n)
		}

		// A second episode warns again.
		f.mu.Lock()
		again := make(chan struct{})
		f.script = []outcome{{hang: again}}
		f.mu.Unlock()
		for i := 200; i <= 302; i++ {
			r.notify(t, "hook", i)
		}
		synctest.Wait()
		if n := r.logs.count("warn", "Notification queue full"); n != 2 {
			t.Fatalf("overflow WARNs after a second episode = %d, want 2", n)
		}
		// 103 events: 200 in flight, 100 queued, 201 and 202 pushed out.
		wantDrops = append(
			wantDrops,
			dropRecord{id: "201", reason: dropOverflow},
			dropRecord{id: "202", reason: dropOverflow},
		)
		if got := r.obs.gotDrops("hook"); !slices.Equal(got, wantDrops) {
			t.Fatalf("drops after a second episode %v, want %v", got, wantDrops)
		}
		close(again)
		synctest.Wait()
		if n := r.logs.count("info", "Notification queue drained"); n != 2 {
			t.Fatalf("drain INFOs = %d, want 2", n)
		}
	})
}

func TestAttemptResetsPerEvent(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fail := outcome{err: status(http.StatusServiceUnavailable, 0)}
		f := newFake("hook", fail, fail, fail, fail, fail)
		r := newRig(t, f)
		start := time.Now()
		r.notify(t, "hook", 1)
		r.notify(t, "hook", 2)
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		// Event 1: 0, 1, 6, 36s, dropped. Event 2: sent at 36s, fails, retried 1s later.
		want := []time.Duration{0, time.Second, 6 * time.Second, 36 * time.Second, 36 * time.Second, 37 * time.Second}
		if got := offsets(start, f.sent()); !slices.Equal(got, want) {
			t.Fatalf("sends at %v, want %v", got, want)
		}
		if got := r.obs.gotDelivered("hook"); !slices.Equal(got, []string{"2"}) {
			t.Fatalf("delivered %v, want [2]", got)
		}
	})
}

func TestStopCancelsInflight(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFake("hook", outcome{block: true})
		r := newRig(t, f)
		r.notify(t, "hook", 1)
		r.notify(t, "hook", 2)
		synctest.Wait()
		start := time.Now()
		stopGroup(t, r.group)
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("Stop waited %v, want 0", waited)
		}
		calls := f.sent()
		if len(calls) != 1 || calls[0].ctx.Err() == nil {
			t.Fatalf("sends %d, first ctx err %v; want 1 cancelled send", len(calls), calls[0].ctx.Err())
		}
		if err := r.runner.Send(
			"hook",
			msg{kind: msgEnqueue, ev: Event{Message: "3"}},
		); !errors.Is(
			err,
			keyed.ErrStopped,
		) {
			t.Fatalf("Send after Stop = %v, want ErrStopped", err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if n := len(f.sent()); n != 1 {
			t.Fatalf("sends after Stop = %d, want 1", n)
		}
	})
}

func TestStopWhileWaiting(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFake("hook", outcome{err: status(http.StatusServiceUnavailable, 0)})
		r := newRig(t, f)
		r.notify(t, "hook", 1)
		synctest.Wait()
		start := time.Now()
		stopGroup(t, r.group)
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("Stop waited %v for a retry timer, want 0", waited)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if n := len(f.sent()); n != 1 {
			t.Fatalf("sends = %d, want 1 (no retry after Stop)", n)
		}
	})
}

func TestSendPanicDropsAndContinues(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFake("hook", outcome{panic: true})
		r := newRig(t, f)
		r.notify(t, "hook", 1)
		r.notify(t, "hook", 2)
		synctest.Wait()
		want := []dropRecord{{id: "1", reason: dropRejected, attempts: 1}}
		if got := r.obs.gotDrops("hook"); !slices.Equal(got, want) {
			t.Fatalf("drops %v, want %v", got, want)
		}
		if got := r.obs.gotDelivered("hook"); !slices.Equal(got, []string{"2"}) {
			t.Fatalf("delivered %v, want [2]", got)
		}
	})
}
