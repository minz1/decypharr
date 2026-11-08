package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

// A handler that never returns cannot hold the shutdown past the stop
// context's deadline: both the grace period and the wait for handlers are
// bounded by it, not by fixed timeouts.
func TestShutdownIsBoundedByTheStopContext(t *testing.T) {
	t.Parallel()
	addr, port := freeAddr(t)
	mgr, store := managertest.New(t, func(cfg *config.Config) {
		cfg.BindAddress = "127.0.0.1"
		cfg.Port = port
	})
	srv := New(mgr, store, logger.Discard(), nil)
	// The grace period alone would outlast the test; only the stop context ends the wait.
	srv.shutdownGrace = time.Hour
	stop, cancelStop := context.WithTimeout(context.WithoutCancel(t.Context()), 300*time.Millisecond)
	defer cancelStop()
	srv.SetStopContext(func() context.Context { return stop })

	g := newGroup(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv.router = chi.NewRouter()
	srv.router.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release // ignores the request context: only the stop deadline can end the wait
		_, _ = io.WriteString(w, "late")
	})
	if err := srv.Start(g); err != nil {
		t.Fatal(err)
	}
	go getWhenListening(t, "http://"+addr+"/")
	<-entered

	started := time.Now()
	g.Close()
	select {
	case <-g.Done():
	case <-time.After(stopTimeout):
		t.Fatal("a stuck handler held the shutdown past the stop deadline")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("shutdown took %v with a 300ms stop deadline", elapsed)
	}
	if err := g.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected failure: %v", err)
	}
}
