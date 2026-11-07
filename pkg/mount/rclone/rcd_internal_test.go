package rclone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
)

// fakeProc is an rcd process whose Wait blocks until it is released.
type fakeProc struct {
	pid     int
	release chan struct{}
	once    sync.Once

	// exitOnSignal makes Signal end the process, as rcd does on SIGINT.
	exitOnSignal bool
	// exitOnKill makes Kill end the process.
	exitOnKill bool
	signalErr  error
	killErr    error

	mu       sync.Mutex
	signals  int
	kills    int
	signalAt time.Time
	waited   bool
	record   func(string)
}

func (p *fakeProc) Wait() error {
	<-p.release
	p.mu.Lock()
	p.waited = true
	p.mu.Unlock()
	return nil
}

func (p *fakeProc) exit() { p.once.Do(func() { close(p.release) }) }

func (p *fakeProc) Signal(os.Signal) error {
	p.mu.Lock()
	p.signals++
	p.signalAt = time.Now()
	p.mu.Unlock()
	p.record("signal")
	if p.exitOnSignal {
		p.exit()
	}
	return p.signalErr
}

func (p *fakeProc) Kill() error {
	p.mu.Lock()
	p.kills++
	p.mu.Unlock()
	p.record("kill")
	if p.exitOnKill {
		p.exit()
	}
	return p.killErr
}

func (p *fakeProc) Pid() int { return p.pid }

func (p *fakeProc) stopped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.signals+p.kills > 0
}

func (p *fakeProc) reaped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waited
}

// syncBuffer is a log sink the owner and the test can both touch.
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

// rcdHarness runs an rcdOwner on its own group, with fake processes and pings.
type rcdHarness struct {
	t     *testing.T
	owner *rcdOwner
	group *task.Group
	logs  *syncBuffer

	pingOK atomic.Bool
	pings  atomic.Int64
	// pingFn replaces the default ping (ok or fails by pingOK) when set.
	pingFn func(ctx context.Context) error

	// newProc builds the process for spawn number n (from 0); spawnErr, when it
	// returns an error, fails that spawn.
	newProc  func(n int, p *fakeProc)
	spawnErr func(n int) error

	// afterReady runs inside the onReady hook, with the number of onReady calls so far.
	afterReady func(readyCount int, latest *fakeProc)

	mu         sync.Mutex
	procs      []*fakeProc
	spawnTimes []time.Time
	events     []string

	readyCalls      atomic.Int64
	exitCalls       atomic.Int64
	beforeStopCalls atomic.Int64
	closeCalls      atomic.Int64

	result chan error
}

func newRcdHarness(t *testing.T) *rcdHarness {
	t.Helper()
	h := &rcdHarness{t: t, logs: &syncBuffer{}, result: make(chan error, 1)}
	logger := zerolog.New(h.logs)
	h.group = task.NewRoot(t.Context(), "rcd", logger)
	o := newRcdOwner(h.group, logger)
	o.spawn = h.spawn
	o.ping = h.ping
	o.onReady = func() {
		n := int(h.readyCalls.Add(1))
		if h.afterReady != nil {
			h.afterReady(n, h.latest())
		}
	}
	o.onExit = func() { h.exitCalls.Add(1) }
	o.beforeStop = func(context.Context) {
		h.beforeStopCalls.Add(1)
		h.record("beforeStop")
	}
	o.closeLog = func() error {
		h.closeCalls.Add(1)
		return nil
	}
	h.owner = o
	return h
}

func (h *rcdHarness) record(event string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event)
}

func (h *rcdHarness) eventList() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.events)
}

func (h *rcdHarness) spawn() (rcdProcess, error) {
	h.mu.Lock()
	n := len(h.spawnTimes)
	h.spawnTimes = append(h.spawnTimes, time.Now())
	h.mu.Unlock()
	if h.spawnErr != nil {
		if err := h.spawnErr(n); err != nil {
			return nil, err
		}
	}
	p := &fakeProc{pid: 1000 + n, release: make(chan struct{}), exitOnSignal: true, exitOnKill: true, record: h.record}
	if h.newProc != nil {
		h.newProc(n, p)
	}
	h.mu.Lock()
	h.procs = append(h.procs, p)
	h.mu.Unlock()
	return p, nil
}

func (h *rcdHarness) ping(ctx context.Context) error {
	h.pings.Add(1)
	if h.pingFn != nil {
		return h.pingFn(ctx)
	}
	if h.pingOK.Load() {
		return nil
	}
	return errors.New("not up")
}

func (h *rcdHarness) latest() *fakeProc {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.procs) == 0 {
		return nil
	}
	return h.procs[len(h.procs)-1]
}

func (h *rcdHarness) proc(i int) *fakeProc {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.procs[i]
}

func (h *rcdHarness) spawns() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.spawnTimes)
}

func (h *rcdHarness) spawnTime(i int) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.spawnTimes[i]
}

// start does what the manager does: New to Starting, then the owner task.
func (h *rcdHarness) start() {
	h.t.Helper()
	if !h.owner.transition(rcdNew, rcdStarting) {
		h.t.Fatal("New to Starting refused")
	}
	if err := h.group.Go("owner", func(ctx context.Context) error {
		err := h.owner.run(ctx)
		h.result <- err
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	synctest.Wait()
}

// advance moves the synctest clock by d and lets the owner settle.
func (h *rcdHarness) advance(d time.Duration) {
	<-time.After(d)
	synctest.Wait()
}

// advanceUntil steps the clock one second at a time until the state is want.
func (h *rcdHarness) advanceUntil(want rcdState, limit time.Duration) {
	h.t.Helper()
	for waited := time.Duration(0); h.owner.state.Load() != want; waited += time.Second {
		if waited > limit {
			h.t.Fatalf("state = %v after %v, want %v", h.owner.state.Load(), waited, want)
		}
		h.advance(time.Second)
	}
}

func (h *rcdHarness) wantState(want rcdState) {
	h.t.Helper()
	if got := h.owner.state.Load(); got != want {
		h.t.Fatalf("state = %v, want %v", got, want)
	}
}

// stop stops the group, which cancels the owner's context, and returns what run returned.
func (h *rcdHarness) stop() error {
	h.t.Helper()
	stopped := make(chan error, 1)
	go func() { stopped <- h.group.Stop(h.t.Context()) }()
	runErr := <-h.result
	if err := <-stopped; err != nil {
		h.t.Fatalf("group stop: %v", err)
	}
	return runErr
}

func (h *rcdHarness) wantStopped() {
	h.t.Helper()
	if err := h.stop(); err != nil {
		h.t.Fatalf("run = %v, want nil", err)
	}
	h.wantState(rcdStopped)
	if got := h.closeCalls.Load(); got != 1 {
		h.t.Fatalf("closeLog calls = %d, want 1", got)
	}
}

func TestUnreachableRcdNeverReady(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newRcdHarness(t)
		h.start()
		h.advanceUntil(rcdFailed, 10*time.Minute)

		if got := h.spawns(); got != 1+rcdMaxRestarts {
			t.Fatalf("spawns = %d, want %d", got, 1+rcdMaxRestarts)
		}
		if got := h.owner.restarts.Load(); got != rcdMaxRestarts {
			t.Fatalf("restarts = %d, want %d", got, rcdMaxRestarts)
		}
		if got := h.readyCalls.Load(); got != 0 {
			t.Fatalf("onReady calls = %d, want 0", got)
		}
		for i := range h.spawns() {
			if p := h.proc(i); !p.stopped() || !p.reaped() {
				t.Fatalf("process %d stopped=%v reaped=%v, want both", i, p.stopped(), p.reaped())
			}
		}
		h.wantStopped()
	})
}

func TestReadyAfterPing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newRcdHarness(t)
		h.pingFn = func(context.Context) error {
			if h.pings.Load() <= 2 {
				return errors.New("not up")
			}
			return nil
		}
		h.start()
		h.advance(5 * time.Second)

		h.wantState(rcdReady)
		if got := h.readyCalls.Load(); got != 1 {
			t.Fatalf("onReady calls = %d, want 1", got)
		}
		if got := h.pings.Load(); got != 3 {
			t.Fatalf("pings = %d, want 3", got)
		}
		h.wantStopped()
	})
}

func TestExitWhileReadyRestarts(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newRcdHarness(t)
		h.pingOK.Store(true)
		h.start()
		h.wantState(rcdReady)

		h.proc(0).exit()
		synctest.Wait()
		h.wantState(rcdRestarting)
		if got := h.exitCalls.Load(); got != 1 {
			t.Fatalf("onExit calls = %d, want 1", got)
		}

		h.advance(rcdBackoffBase)
		h.wantState(rcdReady)
		if got := h.readyCalls.Load(); got != 2 {
			t.Fatalf("onReady calls = %d, want 2", got)
		}
		if got := h.owner.restarts.Load(); got != 1 {
			t.Fatalf("restarts = %d, want 1", got)
		}
		h.wantStopped()
	})
}

// exitWhenReady makes every process exit as soon as it is Ready, except the ones hold names.
func exitWhenReady(h *rcdHarness, hold func(n int) bool) {
	h.afterReady = func(ready int, latest *fakeProc) {
		if hold == nil || !hold(ready-1) {
			latest.exit()
		}
	}
}

func TestCrashLoopEndsFailed(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newRcdHarness(t)
		h.pingOK.Store(true)
		exitWhenReady(h, nil)
		h.start()
		h.advanceUntil(rcdFailed, 10*time.Minute)

		if got := h.spawns(); got != 1+rcdMaxRestarts {
			t.Fatalf("spawns = %d, want %d", got, 1+rcdMaxRestarts)
		}
		if got := h.owner.restarts.Load(); got != rcdMaxRestarts {
			t.Fatalf("restarts = %d, want %d", got, rcdMaxRestarts)
		}
		// onExit fires on every exit from Ready, including the one that fails.
		if got := h.exitCalls.Load(); got != 1+rcdMaxRestarts {
			t.Fatalf("onExit calls = %d, want %d", got, 1+rcdMaxRestarts)
		}
		var gaps []time.Duration
		for i := 1; i < h.spawns(); i++ {
			gaps = append(gaps, h.spawnTime(i).Sub(h.spawnTime(i-1)))
		}
		want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
		if !slices.Equal(gaps, want) {
			t.Fatalf("backoffs = %v, want %v", gaps, want)
		}
		h.wantStopped()
	})
}

func TestStableReadyResetsAttempts(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newRcdHarness(t)
		h.pingOK.Store(true)
		const stable = 4 // the fifth process stays up
		exitWhenReady(h, func(n int) bool { return n == stable })
		h.start()
		h.advanceUntil(rcdReady, time.Minute)
		if got := h.spawns(); got != stable+1 {
			t.Fatalf("spawns = %d, want %d", got, stable+1)
		}
		h.advance(rcdStableAfter + time.Second)
		h.wantState(rcdReady)

		h.proc(stable).exit()
		synctest.Wait()
		// Without the reset this exit would be the sixth failure.
		h.wantState(rcdRestarting)
		h.advanceUntil(rcdFailed, 10*time.Minute)
		if got, want := h.spawns(), stable+1+rcdMaxRestarts; got != want {
			t.Fatalf("spawns = %d, want %d", got, want)
		}
		h.wantStopped()
	})
}

func TestStopDuringStartingNeverReady(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newRcdHarness(t)
		h.start()
		h.advance(5 * time.Second)
		h.wantState(rcdStarting)

		h.wantStopped()
		if got := h.readyCalls.Load(); got != 0 {
			t.Fatalf("onReady calls = %d, want 0", got)
		}
		if got := h.beforeStopCalls.Load(); got != 0 {
			t.Fatalf("beforeStop calls = %d, want 0 (the stop did not start from Ready)", got)
		}
		if !h.proc(0).stopped() || !h.proc(0).reaped() {
			t.Fatal("process was not stopped and reaped")
		}
	})
}

func TestStopFromReadyUnmountsBeforeSignal(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newRcdHarness(t)
		h.pingOK.Store(true)
		h.start()
		h.wantState(rcdReady)

		h.wantStopped()
		if got, want := h.eventList(), []string{"beforeStop", "signal"}; !slices.Equal(got, want) {
			t.Fatalf("events = %v, want %v", got, want)
		}
	})
}

func TestExitWhileStoppingDoesNotRestart(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newRcdHarness(t)
		h.pingOK.Store(true)
		h.start()
		h.wantState(rcdReady)

		h.wantStopped()
		if got := h.spawns(); got != 1 {
			t.Fatalf("spawns = %d, want 1", got)
		}
		if got := h.exitCalls.Load(); got != 0 {
			t.Fatalf("onExit calls = %d, want 0 (the process was stopped, it did not crash)", got)
		}
		if got := h.owner.restarts.Load(); got != 0 {
			t.Fatalf("restarts = %d, want 0", got)
		}
	})
}

func TestStopDuringBackoffSpawnsNothing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newRcdHarness(t)
		h.pingOK.Store(true)
		h.start()
		h.proc(0).exit()
		synctest.Wait()
		h.wantState(rcdRestarting)
		h.advance(rcdBackoffBase / 2)

		h.wantStopped()
		if got := h.spawns(); got != 1 {
			t.Fatalf("spawns = %d, want 1", got)
		}
		if got := h.beforeStopCalls.Load(); got != 0 {
			t.Fatalf("beforeStop calls = %d, want 0", got)
		}
	})
}

func TestHangingPingIsBounded(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newRcdHarness(t)
		began := time.Now()
		var mu sync.Mutex
		var worst time.Duration
		var lastEnd time.Time
		h.pingFn = func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			if !ok {
				return errors.New("ping context has no deadline")
			}
			mu.Lock()
			worst = max(worst, time.Until(deadline))
			if deadline.After(lastEnd) && h.spawns() == 1 {
				lastEnd = deadline
			}
			mu.Unlock()
			<-ctx.Done()
			return ctx.Err()
		}
		h.start()
		h.advance(rcdReadyTimeout + time.Second/2)

		mu.Lock()
		defer mu.Unlock()
		if worst > rcdPingTimeout {
			t.Fatalf("a ping was given %v, want at most %v", worst, rcdPingTimeout)
		}
		if lastEnd.After(began.Add(rcdReadyTimeout)) {
			t.Fatalf("a ping ran to %v, past the %v ready timeout", lastEnd.Sub(began), rcdReadyTimeout)
		}
		p := h.proc(0)
		p.mu.Lock()
		signalled := p.signalAt
		p.mu.Unlock()
		if got := signalled.Sub(began); got != rcdReadyTimeout {
			t.Fatalf("process stopped at %v, want %v", got, rcdReadyTimeout)
		}
		h.wantStopped()
	})
}

func TestKillErrorStaysStopping(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newRcdHarness(t)
		h.newProc = func(_ int, p *fakeProc) {
			p.exitOnSignal = false
			p.exitOnKill = false
			p.killErr = errors.New("kill refused")
		}
		h.start()
		h.advance(time.Second)
		began := time.Now()

		h.assertUnreaped(began, h.proc(0))
	})
}

func TestUnreapableProcessStaysStopping(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newRcdHarness(t)
		h.newProc = func(_ int, p *fakeProc) {
			p.exitOnSignal = false
			p.exitOnKill = false
		}
		h.start()
		h.advance(time.Second)
		began := time.Now()

		h.assertUnreaped(began, h.proc(0))
	})
}

// assertUnreaped stops the group while p never exits, and checks the owner
// gives up after the graceful and reap timeouts in Stopping.
func (h *rcdHarness) assertUnreaped(began time.Time, p *fakeProc) {
	h.t.Helper()
	stopped := make(chan error, 1)
	go func() { stopped <- h.group.Stop(h.t.Context()) }()
	// The group's stop waits for the reaper, which waits for the process.
	runErr := <-h.result
	if runErr == nil {
		h.t.Fatal("run = nil, want an error for the process it could not reap")
	}
	if got, want := time.Since(began), rcloneGracefulStop+rcloneReapTimeout; got != want {
		h.t.Fatalf("gave up after %v, want %v", got, want)
	}
	h.wantState(rcdStopping)
	if got := h.readyCalls.Load(); got != 0 {
		h.t.Fatalf("onReady calls = %d, want 0", got)
	}
	if got := h.closeCalls.Load(); got != 1 {
		h.t.Fatalf("closeLog calls = %d, want 1", got)
	}
	out := h.logs.String()
	if !strings.Contains(out, "rcd could not be reaped") || !strings.Contains(out, strconv.Itoa(p.pid)) {
		h.t.Fatalf("log lacks the unreaped error with pid %d:\n%s", p.pid, out)
	}
	// Let the reaper finish so the group, and the bubble, can end.
	p.exit()
	if err := <-stopped; err != nil {
		h.t.Fatalf("group stop: %v", err)
	}
}

func TestSpawnNotFoundFailsAtOnce(t *testing.T) {
	t.Parallel()
	for name, spawnErr := range map[string]error{
		"not found":  fmt.Errorf("start rclone: %w", exec.ErrNotFound),
		"permission": fmt.Errorf("start rclone: %w", fs.ErrPermission),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				h := newRcdHarness(t)
				h.spawnErr = func(int) error { return spawnErr }
				h.start()
				h.wantState(rcdFailed)
				h.advance(time.Minute)
				h.wantState(rcdFailed)
				if got := h.spawns(); got != 1 {
					t.Fatalf("spawns = %d, want 1", got)
				}
				h.wantStopped()
			})
		})
	}
}
