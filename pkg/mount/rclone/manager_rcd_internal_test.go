package rclone

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/rclone"
	"github.com/sirrobot01/decypharr/internal/task"
)

// handlerTransport serves requests with an in-process handler, so the RC
// client needs no socket and works inside a synctest bubble.
type handlerTransport struct {
	handler http.Handler
	// down, when set and true, refuses every request like a dead listener.
	down *atomic.Bool
}

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.down != nil && t.down.Load() {
		return nil, errors.New("connection refused")
	}
	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)
	res := rec.Result()
	res.Request = req
	return res, nil
}

// managerRig is a Manager whose rcd is a fake process and whose RC server is a handler.
type managerRig struct {
	t     *testing.T
	m     *Manager
	g     *task.Group
	spawn atomic.Int64

	mu     sync.Mutex
	events []string
	procs  []*fakeProc

	// spawnErr fails every spawn when set.
	spawnErr error
	// closes counts closeLog calls.
	closes atomic.Int64
	// fail maps an RC command to the HTTP status it answers with, guarded by mu.
	fail map[string]int
	// mountGate, when set before start, holds every mount/mount call until it is closed.
	mountGate chan struct{}
	// rcDown makes the RC server refuse connections.
	rcDown atomic.Bool
	// hangUnmount makes mount/unmount block until its request is cancelled.
	hangUnmount atomic.Bool
	// detaches records, per forced unmount, whether its ctx was still live.
	detaches []bool
	// stopDetaches and stopEvents are len(detaches) and len(events) at markStop; guarded by mu.
	stopDetaches, stopEvents int
	// mounted is rclone's mount list: set by a mount/mount that answers, cleared by mount/unmount.
	mounted atomic.Bool

	// watching is set by watchOn: a fake mount table watcher runs and readTable reads table.
	watching bool
	// changedCh feeds the fake watcher's changed calls; failCh ends it with an error.
	changedCh chan struct{}
	failCh    chan error
	// changed is the callback the fake watcher was given, guarded by mu.
	changed func()
	// table is the fake mount table's FUSE count at our path, tableErr its read error; guarded by mu.
	table    int
	tableErr error
	// tableIgnoresMount keeps a mount/mount from showing in the fake table.
	tableIgnoresMount bool
	// readGate, when set, holds the next readTable call until it is closed; guarded by mu.
	readGate chan struct{}
	// backend is handed to SetBackendStopped; closeBackend closes it.
	backend chan struct{}
}

// detach is the Manager's forced unmount: it records the ctx's state at call time.
func (r *managerRig) detach(ctx context.Context, _ string) error {
	r.record("detach")
	r.mu.Lock()
	defer r.mu.Unlock()
	// Live means the ctx has not ended and keeps the share the RC call left over.
	deadline, ok := ctx.Deadline()
	r.detaches = append(
		r.detaches,
		ctx.Err() == nil && ok && time.Until(deadline) >= rcdUnmountBudget-rcdRCUnmountBudget,
	)
	return nil
}

// markStop notes where a stop begins: the first mount attempt cleans up too, so the stop's checks look only at what
// came after.
func (r *managerRig) markStop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopDetaches, r.stopEvents = len(r.detaches), len(r.events)
}

// stopCount is how many times event was recorded since markStop.
func (r *managerRig) stopCount(event string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events[r.stopEvents:] {
		if e == event {
			n++
		}
	}
	return n
}

// wantLiveDetach fails unless exactly one forced unmount ran since markStop and its ctx was live.
func (r *managerRig) wantLiveDetach() {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if d := r.detaches[r.stopDetaches:]; len(d) != 1 || !d[0] {
		r.t.Fatalf(
			"forced unmounts since the stop (ctx live?) = %v, want exactly one with a live ctx; events: %v",
			d,
			r.events,
		)
	}
}

// failRC makes the RC command cmd answer with status.
func (r *managerRig) failRC(cmd string, status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail == nil {
		r.fail = make(map[string]int)
	}
	r.fail[cmd] = status
}

func (r *managerRig) failStatus(cmd string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fail[cmd]
}

func newManagerRig(t *testing.T) *managerRig {
	t.Helper()
	cfg := loadConfig(t, `{"mount": {"mount_path": "`+filepath.ToSlash(t.TempDir())+`"}}`)
	r := &managerRig{t: t}
	m := &Manager{
		logger:    zerolog.Nop(),
		mount:     cfg.Mount,
		webdavURL: "http://localhost/webdav/",
	}
	m.client = rclone.NewClient(
		"http://rc.invalid", "", "", nil, zerolog.Nop(),
		rclone.WithTransport(handlerTransport{handler: http.HandlerFunc(r.serveRC), down: &r.rcDown}),
	)
	m.wire()
	m.detach = r.detach
	// By default there are no mount table events: the supervisor keeps today's probe cadence.
	m.watchTable = func(context.Context, func()) error { return errNoMountTable }
	m.readTable = func(string) (int, error) { return 0, errNoMountTable }
	m.owner.spawn = r.spawnProc
	m.owner.closeLog = func() error {
		r.closes.Add(1)
		return nil
	}
	r.backend = make(chan struct{})
	m.SetBackendStopped(r.backend)
	r.m = m
	r.g = task.NewRoot(t.Context(), "mount", zerolog.Nop())
	return r
}

func (r *managerRig) serveRC(w http.ResponseWriter, req *http.Request) {
	cmd := strings.TrimPrefix(req.URL.Path, "/")
	r.record("rc:" + cmd)
	if cmd == "mount/mount" && r.watching && r.failStatus(cmd) == 0 {
		// The kernel mount exists before rclone answers.
		r.plugMount()
	}
	if gate := r.currentMountGate(); cmd == "mount/mount" && gate != nil {
		select {
		case <-gate:
		case <-req.Context().Done():
		}
	}
	if cmd == "mount/unmount" && r.hangUnmount.Load() {
		<-req.Context().Done()
	}
	if status := r.failStatus(cmd); status != 0 {
		http.Error(w, "failed", status)
		return
	}
	switch cmd {
	case "mount/mount":
		if !r.watching && req.Context().Err() == nil {
			r.mounted.Store(true)
		}
	case "mount/unmount":
		r.mounted.Store(false)
		if r.watching {
			r.setTable(0)
		}
	case "mount/listmounts":
		if r.mounted.Load() {
			_, _ = fmt.Fprintf(w, `{"mountPoints":[{"MountPoint":%q}]}`, r.m.mount.MountPath)
		} else {
			_, _ = w.Write([]byte(`{"mountPoints":[]}`))
		}
		return
	default:
	}
	_, _ = w.Write([]byte("{}"))
}

// wantMount fails unless the mount's state is want.
func (r *managerRig) wantMount(want mountState) {
	r.t.Helper()
	if got := r.m.view.Load().state; got != want {
		r.t.Fatalf("mount state = %s, want %s; events: %v", got, want, r.events)
	}
}

func (r *managerRig) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *managerRig) spawnProc() (rcdProcess, error) {
	n := int(r.spawn.Add(1)) - 1
	if r.spawnErr != nil {
		return nil, r.spawnErr
	}
	p := &fakeProc{pid: 2000 + n, release: make(chan struct{}), exitOnSignal: true, exitOnKill: true, record: r.record}
	r.mu.Lock()
	r.procs = append(r.procs, p)
	r.mu.Unlock()
	return p, nil
}

func (r *managerRig) count(event string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e == event {
			n++
		}
	}
	return n
}

func (r *managerRig) index(event string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Index(r.events, event)
}

func (r *managerRig) proc() *fakeProc {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.procs[0]
}

func (r *managerRig) start() {
	r.t.Helper()
	if err := r.m.Start(r.g); err != nil {
		r.t.Fatalf("Start: %v", err)
	}
	synctest.Wait()
}

func (r *managerRig) wantState(want rcdState) {
	r.t.Helper()
	if got := r.m.owner.state.Load(); got != want {
		r.t.Fatalf("state = %v, want %v", got, want)
	}
}

func TestStartTwiceErrors(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.start()
		if err := r.m.Start(r.g); !errors.Is(err, errStarted) {
			t.Fatalf("second Start = %v, want errStarted", err)
		}
		if got := r.spawn.Load(); got != 1 {
			t.Fatalf("spawns = %d, want 1", got)
		}
		r.markStop()
		if err := r.m.Stop(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestStartAfterStopIsStopped(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.start()
		r.markStop()
		if err := r.m.Stop(); err != nil {
			t.Fatal(err)
		}
		if err := r.m.Start(r.g); !errors.Is(err, task.ErrStopped) {
			t.Fatalf("Start after Stop = %v, want task.ErrStopped", err)
		}
	})
}

func TestStopBeforeStartIsNil(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.markStop()
		if err := r.m.Stop(); err != nil {
			t.Fatalf("Stop before Start = %v, want nil", err)
		}
		r.wantState(rcdStopped)
		if err := r.m.Start(r.g); !errors.Is(err, task.ErrStopped) {
			t.Fatalf("Start after Stop = %v, want task.ErrStopped", err)
		}
		if got := r.spawn.Load(); got != 0 {
			t.Fatalf("spawns = %d, want 0", got)
		}
	})
}

func TestConcurrentStartSpawnsOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		const callers = 8
		errs := make(chan error, callers)
		var wg sync.WaitGroup
		for range callers {
			wg.Go(func() { errs <- r.m.Start(r.g) })
		}
		wg.Wait()
		close(errs)
		synctest.Wait()

		succeeded := 0
		for err := range errs {
			switch {
			case err == nil:
				succeeded++
			case !errors.Is(err, errStarted):
				t.Fatalf("Start = %v, want nil or errStarted", err)
			}
		}
		if succeeded != 1 {
			t.Fatalf("%d Start calls succeeded, want exactly 1", succeeded)
		}
		if got := r.spawn.Load(); got != 1 {
			t.Fatalf("spawns = %d, want 1", got)
		}
		r.markStop()
		if err := r.m.Stop(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestStopUnmountsViaRCBeforeSignallingRcd(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.start()
		r.wantState(rcdReady)
		if !r.m.IsMounted() {
			t.Fatal("mount did not come up")
		}

		if err := r.g.Stop(t.Context()); err != nil {
			t.Fatal(err)
		}
		unmounted, signalled := r.index("rc:mount/unmount"), r.index("signal")
		if unmounted < 0 || signalled < 0 || unmounted > signalled {
			t.Fatalf("unmount at %d, signal at %d, want the unmount first; events: %v", unmounted, signalled, r.events)
		}
		r.wantState(rcdStopped)
	})
}

func TestRemountAfterRcdExit(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.start()
		if got := r.count("rc:mount/mount"); got != 1 {
			t.Fatalf("mount calls = %d, want 1", got)
		}

		r.proc().exit()
		synctest.Wait()
		if r.m.IsMounted() {
			t.Fatal("mount still marked mounted after rcd exited")
		}
		if info := r.m.mountInfo(); info == nil || info.Error == "" || info.State != "waiting" {
			t.Fatalf("mount info = %+v, want the exit recorded", info)
		}

		<-time.After(rcdBackoffBase)
		synctest.Wait()
		r.wantState(rcdReady)
		if got := r.count("rc:mount/mount"); got != 2 {
			t.Fatalf("mount calls = %d, want 2", got)
		}
		if !r.m.IsMounted() {
			t.Fatal("mount not restored after rcd restarted")
		}
		r.markStop()
		if err := r.m.Stop(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestStatsShowsStateWhenNotReady(t *testing.T) {
	t.Parallel()
	r := newManagerRig(t)
	r.m.owner.transition(rcdNew, rcdStarting)
	r.m.owner.transition(rcdStarting, rcdFailed)

	stats := r.m.Stats()

	if got := stats["state"]; got != "failed" {
		t.Fatalf("state = %v, want failed", got)
	}
	if got := stats["ready"]; got != false {
		t.Fatalf("ready = %v, want false", got)
	}
	if _, ok := stats["restarts"]; !ok {
		t.Fatal("stats lack restarts")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) != 0 {
		t.Fatalf("RC calls while rcd is not ready: %v", r.events)
	}
}

func TestStopLeavesNoTasks(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.start()
		if len(r.g.Snapshot()) == 0 {
			t.Fatal("no tasks while running")
		}
		r.markStop()
		if err := r.m.Stop(); err != nil {
			t.Fatal(err)
		}
		if left := r.g.Snapshot(); len(left) != 0 {
			t.Fatalf("tasks left after Stop: %v", left)
		}
	})
}

func TestStopAfterExitStillUnmountsViaRC(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.start()
		r.proc().exit()
		synctest.Wait()
		// The next Ready's mount attempts fail, so the mount stays marked down.
		r.failRC("mount/mount", http.StatusBadRequest)
		<-time.After(rcdBackoffBase)
		synctest.Wait()
		r.wantState(rcdReady)
		if r.m.IsMounted() {
			t.Fatal("mount should be marked down")
		}

		r.markStop()
		if err := r.m.Stop(); err != nil {
			t.Fatal(err)
		}
		if got := r.count("rc:mount/unmount"); got != 1 {
			t.Fatalf("unmount calls = %d, want 1; events: %v", got, r.events)
		}
		r.wantMount(mountUnmounted)
	})
}

func TestStopClosesLog(t *testing.T) {
	t.Parallel()
	for name, failed := range map[string]bool{"ready": false, "failed": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				r := newManagerRig(t)
				if failed {
					r.spawnErr = fmt.Errorf("start rclone: %w", exec.ErrNotFound)
				}
				r.start()
				if failed {
					r.wantState(rcdFailed)
				} else {
					r.wantState(rcdReady)
				}
				if err := r.m.Stop(); err != nil {
					t.Fatal(err)
				}
				if got := r.closes.Load(); got != 1 {
					t.Fatalf("closeLog calls = %d, want 1", got)
				}
			})
		})
	}
}

// TestHelperRcd is the child of TestRealRcdProcessIsReaped: it behaves like rcd
// and exits cleanly on SIGINT.
func TestHelperRcd(t *testing.T) {
	t.Parallel()
	if os.Getenv("GO_WANT_HELPER_RCD") != "1" {
		t.Skip("helper process")
	}
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt)
	_, _ = os.Stdout.WriteString("ready\n")
	<-interrupted
	os.Exit(0)
}

// A real process, outside any synctest bubble: Signal reaches it and Wait returns.
func TestRealRcdProcessIsReaped(t *testing.T) {
	t.Parallel()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperRcd$")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_RCD=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if startErr := cmd.Start(); startErr != nil {
		t.Fatal(startErr)
	}
	proc := &execProcess{cmd: cmd}
	if proc.Pid() <= 0 {
		t.Fatalf("pid = %d", proc.Pid())
	}
	// The helper prints once its handler is installed, so the signal cannot kill it early.
	if line, readErr := bufio.NewReader(out).ReadString('\n'); readErr != nil || !strings.HasPrefix(line, "ready") {
		t.Fatalf("helper said %q, %v", line, readErr)
	}

	var _ rcdProcess = proc
	if sigErr := proc.Signal(os.Interrupt); sigErr != nil {
		t.Fatal(sigErr)
	}
	if waitErr := proc.Wait(); waitErr != nil {
		t.Fatalf("Wait = %v, want a clean exit", waitErr)
	}
}

// rcd died and the owner waits out the backoff: the dead rcd's mount is still
// there, and the stop must clear it.
func TestStopDuringBackoffUnmounts(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.start()
		r.proc().exit()
		synctest.Wait()
		r.wantState(rcdRestarting)

		r.markStop()
		if err := r.m.Stop(); err != nil {
			t.Fatal(err)
		}
		if got := r.count("rc:mount/unmount"); got != 0 {
			t.Fatalf("RC unmount calls = %d, want 0: rcd is dead; events: %v", got, r.events)
		}
		r.wantLiveDetach()
		r.wantState(rcdStopped)
	})
}

// A crash loop ends in Failed with the last mount still in place.
func TestStopAfterCrashLoopUnmounts(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.start()
		r.spawnErr = errors.New("rclone crashes")
		r.proc().exit()
		synctest.Wait()
		<-time.After(5 * time.Minute)
		synctest.Wait()
		r.wantState(rcdFailed)

		r.markStop()
		if err := r.m.Stop(); err != nil {
			t.Fatal(err)
		}
		if got := r.count("rc:mount/unmount"); got != 0 {
			t.Fatalf("RC unmount calls = %d, want 0: rcd is dead; events: %v", got, r.events)
		}
		r.wantLiveDetach()
	})
}

// The RC refuses connections while the manager is Ready: the retries eat their
// share, and the forced unmount still gets a live ctx.
func TestStopWithRefusingRCLeavesForcedUnmountTime(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.start()
		r.wantState(rcdReady)
		r.rcDown.Store(true)

		r.markStop()
		if err := r.m.Stop(); err != nil {
			t.Fatal(err)
		}
		r.wantLiveDetach()
		r.wantState(rcdStopped)
	})
}

// The RC hangs while the manager is Ready: the RC call is cut at its own
// budget, and the forced unmount still gets a live ctx.
func TestStopWithHangingRCLeavesForcedUnmountTime(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.start()
		r.wantState(rcdReady)
		r.hangUnmount.Store(true)

		r.markStop()
		if err := r.m.Stop(); err != nil {
			t.Fatal(err)
		}
		r.wantLiveDetach()
		r.wantState(rcdStopped)
	})
}

// Against a dead rcd the RC unmount fails; the stop must still finish inside the unmount budget.
func TestStopWithDeadRCFinishesWithinBudget(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.start()
		r.proc().exit()
		synctest.Wait()
		r.failRC("mount/unmount", http.StatusInternalServerError)

		began := time.Now()
		r.markStop()
		if err := r.m.Stop(); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(began); took > rcdUnmountBudget+time.Second {
			t.Fatalf("Stop took %v, want within the %v unmount budget", took, rcdUnmountBudget)
		}
		r.wantState(rcdStopped)
	})
}
