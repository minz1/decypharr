package server

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

// The server's whole drain has a slice of the stop budget of its own. A handler that ignores the forced close
// cannot hold the shutdown to the generation's stop deadline, which would leave the manager no time to stop.
func TestShutdownFitsItsSliceOfTheStopBudget(t *testing.T) {
	t.Parallel()
	addr, port := freeAddr(t)
	mgr, store := managertest.New(t, func(cfg *config.Config) {
		cfg.BindAddress = "127.0.0.1"
		cfg.Port = port
	})
	srv := New(mgr, store, logger.Discard(), nil)
	srv.shutdownGrace = 50 * time.Millisecond
	srv.drainSlice = 300 * time.Millisecond
	// The stop deadline is far away: only the server's own slice can end the wait.
	stop, cancelStop := context.WithTimeout(context.WithoutCancel(t.Context()), time.Hour)
	defer cancelStop()
	srv.SetStopContext(func() context.Context { return stop })

	g := newGroup(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv.router = chi.NewRouter()
	srv.router.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
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
		t.Fatal("a stuck handler held the shutdown past the server's slice of the stop budget")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("shutdown took %v with a 300ms slice", elapsed)
	}
}
