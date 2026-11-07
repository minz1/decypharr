package rclone

import (
	"bufio"
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
type handlerTransport struct{ handler http.Handler }

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
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
		rclone.WithTransport(handlerTransport{handler: http.HandlerFunc(r.serveRC)}),
	)
	m.wire()
	m.owner.spawn = r.spawnProc
	m.owner.closeLog = func() error {
		r.closes.Add(1)
		return nil
	}
	r.m = m
	r.g = task.NewRoot(t.Context(), "mount", zerolog.Nop())
	return r
}

func (r *managerRig) serveRC(w http.ResponseWriter, req *http.Request) {
	r.record("rc:" + strings.TrimPrefix(req.URL.Path, "/"))
	_, _ = w.Write([]byte("{}"))
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

func (r *managerRig) proc(i int) *fakeProc {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.procs[i]
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

		r.proc(0).exit()
		synctest.Wait()
		if r.m.IsMounted() {
			t.Fatal("mount still marked mounted after rcd exited")
		}
		if info := r.m.getMountInfo(); info == nil || info.Error == "" {
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
		if err := r.m.Stop(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRemountSkippedDuringRecovery(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.m.recovering.Store(true)
		r.start()
		r.wantState(rcdReady)
		if got := r.count("rc:mount/mount"); got != 0 {
			t.Fatalf("mount calls = %d, want 0 while a recovery runs", got)
		}
		if err := r.m.Stop(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestHealthCheckSkipsWhenNotReady(t *testing.T) {
	t.Parallel()
	r := newManagerRig(t)
	r.m.owner.transition(rcdNew, rcdStarting)
	r.m.owner.transition(rcdStarting, rcdRestarting)
	r.m.info.Store(&MountInfo{LocalPath: "/mnt/x", Mounted: true})

	r.m.performMountHealthCheck(t.Context())

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) != 0 {
		t.Fatalf("RC calls while rcd restarts: %v", r.events)
	}
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
		r.proc(0).exit()
		synctest.Wait()
		// The next Ready finds a recovery running, so the mount stays marked down.
		r.m.recovering.Store(true)
		<-time.After(rcdBackoffBase)
		synctest.Wait()
		r.wantState(rcdReady)
		if r.m.IsMounted() {
			t.Fatal("mount should be marked down")
		}

		if err := r.m.Stop(); err != nil {
			t.Fatal(err)
		}
		if got := r.count("rc:mount/unmount"); got != 1 {
			t.Fatalf("unmount calls = %d, want 1", got)
		}
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
