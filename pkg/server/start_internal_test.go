package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

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
	done := make(chan error, 1)
	go func() { done <- srv.Start(t.Context()) }()
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("Start returned nil for a busy port")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start kept running without a listener")
	}
}

// With a free port Start serves until its context ends, then returns nil.
func TestStartStopsWithContext(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, func(cfg *config.Config) {
		cfg.BindAddress = "127.0.0.1"
		cfg.Port = "0"
	})
	srv := New(mgr, store, logger.Discard())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
}

// A handler that triggers a restart still gets its response out: shutdown waits for it.
func TestStartFinishesInFlightResponseOnStop(t *testing.T) {
	t.Parallel()
	free, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := free.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	_ = free.Close()

	mgr, store := managertest.New(t, func(cfg *config.Config) {
		cfg.BindAddress = "127.0.0.1"
		cfg.Port = port
	})
	srv := New(mgr, store, logger.Discard())
	ctx, cancel := context.WithCancel(t.Context())
	release := make(chan struct{})
	srv.router = chi.NewRouter()
	srv.router.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		<-release
		_, _ = io.WriteString(w, "restarting")
	})
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	body := make(chan string, 1)
	go func() { body <- getWhenListening(t, "http://"+addr+"/") }()

	<-ctx.Done()
	select {
	case <-done:
		t.Fatal("Start returned while a handler was still writing")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if got := <-body; got != "restarting" {
		t.Fatalf("body = %q, want the handler's response", got)
	}
	if err = <-done; err != nil {
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
	free, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := free.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	_ = free.Close()

	mgr, store := managertest.New(t, func(cfg *config.Config) {
		cfg.BindAddress = "127.0.0.1"
		cfg.Port = port
	})
	srv := New(mgr, store, logger.Discard())
	srv.shutdownGrace = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	srv.router = chi.NewRouter()
	exited := make(chan struct{})
	srv.router.Get("/", func(w http.ResponseWriter, r *http.Request) {
		defer close(exited)
		cancel()
		_, _ = io.WriteString(w, "streaming")
		http.NewResponseController(w).Flush()
		<-r.Context().Done()
		time.Sleep(100 * time.Millisecond)
	})
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()
	go getWhenListening(t, "http://"+addr+"/")

	<-ctx.Done()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an open stream held Start past the shutdown grace period")
	}
	select {
	case <-exited:
	default:
		t.Fatal("Start returned while a closed stream's handler was still running")
	}
}
