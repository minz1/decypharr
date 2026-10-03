package nntp

import (
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
)

// The janitor belongs to its Client: its sweeper starts with the first
// connection and stops at close, and nothing registers afterwards.
func TestBodyJanitorStopsAtClose(t *testing.T) {
	t.Parallel()
	janitor := newBodyJanitor()
	conn := newPipeConnection(t, true)
	janitor.add(conn)

	closed := make(chan struct{})
	go func() {
		janitor.close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not stop the sweeper")
	}
	select {
	case <-janitor.done:
	default:
		t.Fatal("sweeper goroutine still running after close")
	}

	janitor.add(conn)
	janitor.mu.Lock()
	registered := len(janitor.conns)
	running := janitor.running
	janitor.mu.Unlock()
	if registered != 0 {
		t.Fatalf("%d connections registered after close", registered)
	}
	if !running {
		t.Fatal("janitor forgot it had started")
	}
	janitor.close() // idempotent
}

// A janitor that never saw a connection closes without a goroutine to wait on.
func TestUnusedBodyJanitorCloses(t *testing.T) {
	t.Parallel()
	janitor := newBodyJanitor()
	janitor.close()
	var nilJanitor *bodyJanitor
	nilJanitor.add(nil)
	nilJanitor.remove(nil)
	nilJanitor.close()
}

// Pools and connections share their Client's clock, so cooldown deadlines
// and progress stamps compare across them.
func TestClientSharesClockWithPools(t *testing.T) {
	t.Parallel()
	pools, ordered := buildPools([]config.UsenetProvider{
		{Host: "a.example", Port: 563, MaxConnections: 1},
		{Host: "b.example", Port: 563, MaxConnections: 1},
	}, newMonoClock())
	if len(pools) == 0 || len(ordered) == 0 {
		t.Fatal("no pools built")
	}
	first := ordered[0].clock
	for _, pp := range ordered {
		if !pp.clock.epoch.Equal(first.epoch) {
			t.Fatal("pools built with different clocks")
		}
	}
	if now := first.now(); now < 0 {
		t.Fatalf("clock reads %d before its epoch", now)
	}
}
