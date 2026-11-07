package external

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/rclone"
	"github.com/sirrobot01/decypharr/internal/state/statetest"
	"github.com/sirrobot01/decypharr/internal/task"
)

func newTestManager(ping func(context.Context) error) *Manager {
	client := rclone.NewClient("http://rc.invalid", "", "", nil, zerolog.Nop(), rclone.WithTransport(emptyRC{}))
	m := newManager(client, zerolog.Nop(), true)
	m.ping = ping
	return m
}

func TestExternalTransitionTable(t *testing.T) {
	t.Parallel()
	statetest.Table(t, extSuccessors,
		[]extState{extNew, extStarting, extReady, extFailed, extStopped},
		[]statetest.Edge[extState]{
			{From: extNew, To: extStarting},
			{From: extNew, To: extStopped},
			{From: extStarting, To: extReady},
			{From: extStarting, To: extFailed},
			{From: extStarting, To: extStopped},
			{From: extReady, To: extStopped},
			{From: extFailed, To: extStopped},
		})
}

func TestExternalStateNames(t *testing.T) {
	t.Parallel()
	want := map[extState]string{
		extNew: "new", extStarting: "starting", extReady: "ready", extFailed: "failed", extStopped: "stopped",
	}
	for s, name := range want {
		if got := s.String(); got != name {
			t.Errorf("state %d = %q, want %q", uint32(s), got, name)
		}
	}
}

// Start used to report ready without asking the RC server anything.
func TestUnreachableRCFails(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var pings atomic.Int32
		var times []time.Duration
		begin := time.Now()
		m := newTestManager(func(context.Context) error {
			pings.Add(1)
			times = append(times, time.Since(begin))
			return errors.New("connection refused")
		})
		g := task.NewRoot(t.Context(), "mount", zerolog.Nop())
		if err := m.Start(g); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		<-time.After(10 * time.Second)
		synctest.Wait()

		if m.IsReady() {
			t.Fatal("IsReady with an RC server that never answered")
		}
		if got := m.state.Load(); got != extFailed {
			t.Fatalf("state = %v, want failed", got)
		}
		if got := pings.Load(); got != 3 {
			t.Fatalf("pings = %d, want 3", got)
		}
		want := []time.Duration{0, 3 * time.Second, 6 * time.Second}
		for i := range want {
			if times[i] != want[i] {
				t.Fatalf("pings at %v, want %v", times, want)
			}
		}
		if err := g.Stop(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRCUpOnSecondPingIsReady(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var pings atomic.Int32
		m := newTestManager(func(context.Context) error {
			if pings.Add(1) < 2 {
				return errors.New("not yet")
			}
			return nil
		})
		g := task.NewRoot(t.Context(), "mount", zerolog.Nop())
		if err := m.Start(g); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if m.IsReady() {
			t.Fatal("ready after the first, failed ping")
		}
		<-time.After(probeInterval)
		synctest.Wait()

		if !m.IsReady() {
			t.Fatalf("state = %v, want ready", m.state.Load())
		}
		if got := m.Stats()["state"]; got != "ready" {
			t.Fatalf("stats state = %v, want ready", got)
		}
		if err := g.Stop(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestExternalStopEndsProbe(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := newTestManager(func(context.Context) error { return errors.New("down") })
		g := task.NewRoot(t.Context(), "mount", zerolog.Nop())
		if err := m.Start(g); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()

		if err := g.Stop(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := m.state.Load(); got != extStopped {
			t.Fatalf("state = %v, want stopped", got)
		}
		if left := g.Snapshot(); len(left) != 0 {
			t.Fatalf("tasks left: %v", left)
		}
	})
}

func TestStartWithWebDAVDisabledErrors(t *testing.T) {
	t.Parallel()
	m := newTestManager(func(context.Context) error { return nil })
	m.webdavEnabled = false
	if err := m.Start(task.NewRoot(t.Context(), "mount", zerolog.Nop())); err == nil {
		t.Fatal("Start succeeded with WebDAV disabled")
	}
}

func TestRefreshBeforeStartErrors(t *testing.T) {
	t.Parallel()
	m := newTestManager(func(context.Context) error { return nil })
	if err := m.Refresh([]string{""}); !errors.Is(err, errNotStarted) {
		t.Fatalf("Refresh before Start = %v, want errNotStarted", err)
	}
	stats := m.Stats()
	if stats["ready"] != false || stats["state"] != "new" {
		t.Fatalf("stats before Start = %v", stats)
	}
}

// emptyRC answers every RC call with an empty JSON object, so a client under
// synctest makes no network call.
type emptyRC struct{}

func (emptyRC) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("{}")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestStartAfterStopSaysStopped(t *testing.T) {
	t.Parallel()
	m := newTestManager(func(context.Context) error { return nil })
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}

	err := m.Start(task.NewRoot(t.Context(), "mount", zerolog.Nop()))

	if !errors.Is(err, task.ErrStopped) {
		t.Fatalf("Start after Stop = %v, want task.ErrStopped", err)
	}
}
