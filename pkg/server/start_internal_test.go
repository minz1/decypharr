package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

// stopTimeout bounds every wait on a group in these tests.
const stopTimeout = 10 * time.Second

func newGroup(t *testing.T) *task.Group {
	t.Helper()
	g := task.NewRoot(t.Context(), "test", zerolog.Nop())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), stopTimeout)
		defer cancel()
		if err := g.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return g
}

func stopGroup(t *testing.T, g *task.Group) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), stopTimeout)
	defer cancel()
	if err := g.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func freeAddr(t *testing.T) (string, string) {
	t.Helper()
	free, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := free.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	_ = free.Close()
	return addr, port
}

// A port that cannot be bound fails Start instead of leaving the process
// running without HTTP.
func TestStartReportsListenFailure(t *testing.T) {
	t.Parallel()
	busy, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = busy.Close() })
	_, port, _ := net.SplitHostPort(busy.Addr().String())

	mgr, store := managertest.New(t, func(cfg *config.Config) {
		cfg.BindAddress = "127.0.0.1"
		cfg.Port = port
	})
	srv := New(mgr, store, logger.Discard())
	g := newGroup(t)
	if err = srv.Start(g); err == nil {
		t.Fatal("Start returned nil for a busy port")
	}
	if tasks := g.Snapshot(); len(tasks) != 0 {
		t.Fatalf("a failed Start left tasks running: %v", tasks)
	}
}

// With a free port Start serves on its group until the group stops; nothing
// is left running and the group is not failed.
func TestStartServesUntilItsGroupStops(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, func(cfg *config.Config) {
		cfg.BindAddress = "127.0.0.1"
		cfg.Port = "0"
	})
	srv := New(mgr, store, logger.Discard())
	g := newGroup(t)
	if err := srv.Start(g); err != nil {
		t.Fatal(err)
	}
	if tasks := g.Snapshot(); len(tasks) != 3 {
		t.Fatalf("tasks = %v, want stats, the serve loop and its shutdown watcher", tasks)
	}
	stopGroup(t, g)
	if err := g.Err(); err != nil {
		t.Fatalf("group failed: %v", err)
	}
	if tasks := g.Snapshot(); len(tasks) != 0 {
		t.Fatalf("tasks left after Stop: %v", tasks)
	}
}

// Start on a group that is already stopped starts nothing and says why.
func TestStartOnAStoppedGroupRefuses(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, func(cfg *config.Config) {
		cfg.BindAddress = "127.0.0.1"
		cfg.Port = "0"
	})
	srv := New(mgr, store, logger.Discard())
	g := newGroup(t)
	stopGroup(t, g)
	if err := srv.Start(g); err == nil {
		t.Fatal("Start succeeded on a stopped group")
	}
}

// A handler that triggers a restart still gets its response out: shutdown waits for it.
func TestStartFinishesInFlightResponseOnStop(t *testing.T) {
	t.Parallel()
	addr, port := freeAddr(t)
	mgr, store := managertest.New(t, func(cfg *config.Config) {
		cfg.BindAddress = "127.0.0.1"
		cfg.Port = port
	})
	srv := New(mgr, store, logger.Discard())
	g := newGroup(t)
	release := make(chan struct{})
	srv.router = chi.NewRouter()
	srv.router.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		g.Close()
		<-release
		_, _ = io.WriteString(w, "restarting")
	})
	if err := srv.Start(g); err != nil {
		t.Fatal(err)
	}

	body := make(chan string, 1)
	go func() { body <- getWhenListening(t, "http://"+addr+"/") }()

	<-g.Context().Done()
	select {
	case <-g.Done():
		t.Fatal("the group finished while a handler was still writing")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if got := <-body; got != "restarting" {
		t.Fatalf("body = %q, want the handler's response", got)
	}
	stopGroup(t, g)
	if err := g.Err(); err != nil {
		t.Fatal(err)
	}
}

func getWhenListening(t *testing.T, url string) string {
	t.Helper()
	for {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			return err.Error()
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if t.Context().Err() != nil {
				return err.Error()
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return string(b)
	}
}

// An open stream cannot hold a restart or stop: after the grace period Start closes it.
func TestStartClosesStreamsThatOutliveShutdownGrace(t *testing.T) {
	t.Parallel()
	addr, port := freeAddr(t)
	mgr, store := managertest.New(t, func(cfg *config.Config) {
		cfg.BindAddress = "127.0.0.1"
		cfg.Port = port
	})
	srv := New(mgr, store, logger.Discard())
	srv.shutdownGrace = 100 * time.Millisecond
	g := newGroup(t)
	srv.router = chi.NewRouter()
	exited := make(chan struct{})
	srv.router.Get("/", func(w http.ResponseWriter, r *http.Request) {
		defer close(exited)
		g.Close()
		_, _ = io.WriteString(w, "streaming")
		http.NewResponseController(w).Flush()
		<-r.Context().Done()
		time.Sleep(100 * time.Millisecond)
	})
	if err := srv.Start(g); err != nil {
		t.Fatal(err)
	}
	go getWhenListening(t, "http://"+addr+"/")

	select {
	case <-g.Done():
	case <-time.After(stopTimeout):
		t.Fatal("an open stream held the group past the shutdown grace period")
	}
	if err := g.Err(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("the group finished while a closed stream's handler was still running")
	}
}
