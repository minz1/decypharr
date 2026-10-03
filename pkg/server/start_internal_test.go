package server

import (
	"context"
	"net"
	"testing"
	"time"

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
