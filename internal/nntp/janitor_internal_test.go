package nntp

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

// The janitor belongs to its Client's group: its sweeper is a task of that
// group, and a connection registered after the group stopped is closed
// rather than left unwatched.
func TestBodyJanitorStopsWithItsGroup(t *testing.T) {
	t.Parallel()
	g := newTestGroup(t)
	janitor := newBodyJanitor(g)
	conn := newPipeConnection(t, true)
	janitor.add(conn)

	g.Close()
	<-g.Done()
	if left := g.Snapshot(); len(left) != 0 {
		t.Fatalf("tasks still running after stop: %v", left)
	}

	late := newPipeConnection(t, true)
	janitor.add(late)
	janitor.mu.Lock()
	registered := len(janitor.conns)
	stopped := janitor.stopped
	janitor.mu.Unlock()
	if registered != 0 {
		t.Fatalf("%d connections registered after stop", registered)
	}
	if !stopped {
		t.Fatal("janitor did not record that it stopped")
	}
	if _, err := late.conn.Write([]byte("x")); err == nil {
		t.Fatal("a connection added after stop was left open and unwatched")
	}
}

// A janitor on a group that refuses tasks is born stopped, and a nil janitor
// ignores everything.
func TestStoppedGroupJanitorIsBornStopped(t *testing.T) {
	t.Parallel()
	g := newTestGroup(t)
	g.Close()
	<-g.Done()
	janitor := newBodyJanitor(g)
	conn := newPipeConnection(t, true)
	janitor.add(conn)
	if _, err := conn.conn.Write([]byte("x")); err == nil {
		t.Fatal("a stopped janitor kept a connection open")
	}
	var nilJanitor *bodyJanitor
	nilJanitor.add(nil)
	nilJanitor.remove(nil)
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
