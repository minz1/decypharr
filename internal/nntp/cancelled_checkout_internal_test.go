package nntp

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A checkout whose caller has already gone gets no verdict on the stale
// pooled connection it popped. That connection may be healthy: it goes back
// on the idle stack, open, instead of being closed.
func TestCancelledCheckoutKeepsTheIdleConnection(t *testing.T) {
	t.Parallel()
	pp := newTestPool(2)
	conn := newPipeConnection(t, true)
	entry := poolEntry(pp, conn, 90*time.Second) // past staleThreshold (60s), inside idleTimeout (5m)
	c := newAcquireTestClient(t, pp)
	lastUsed := entry.lastUsed

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, popped, err := c.popIdle(ctx, pp)
	if got != nil {
		t.Fatalf("a cancelled checkout handed out a connection")
	}
	if !popped || !errors.Is(err, context.Canceled) {
		t.Fatalf("popIdle = popped %v, err %v; want popped, context.Canceled", popped, err)
	}
	if conn.IsClosed() {
		t.Fatal("the cancelled checkout closed the pooled connection")
	}
	pp.mu.Lock()
	defer pp.mu.Unlock()
	if len(pp.conns) != 1 || pp.conns[0].conn != conn {
		t.Fatalf("idle connections after a cancelled checkout = %d, want the same 1", len(pp.conns))
	}
	if !pp.conns[0].lastUsed.Equal(lastUsed) {
		t.Fatal("the returned connection's last use was reset: it would skip its next verify ping")
	}
}
