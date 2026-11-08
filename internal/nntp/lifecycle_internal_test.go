package nntp

import (
	"bufio"
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
)

// greetingServer accepts one connection and sends nothing: a dial to it sits in
// the greeting read. sawClose is closed once the server side reads EOF or an
// error, which is how the test observes the client closing the socket.
type greetingServer struct {
	accepted chan struct{}
	sawClose chan struct{}
	port     int
}

func startGreetingServer(t *testing.T, greeting string) *greetingServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &greetingServer{
		accepted: make(chan struct{}),
		sawClose: make(chan struct{}),
		port:     ln.Addr().(*net.TCPAddr).Port,
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		close(s.accepted)
		if greeting != "" {
			_, _ = conn.Write([]byte(greeting))
		}
		buf := make([]byte, 64)
		for {
			if _, readErr := conn.Read(buf); readErr != nil {
				close(s.sawClose)
				return
			}
		}
	}()
	return s
}

func lifecycleConfig(port int) *config.Config {
	return &config.Config{Usenet: config.Usenet{Providers: []config.UsenetProvider{
		{Host: "127.0.0.1", Port: port, MaxConnections: 2},
	}}}
}

const lifecycleStopBudget = 2 * time.Second

// startLifecycleClient builds a client on its own root, which the test can stop
// and which is stopped again, harmlessly, when the test ends.
func startLifecycleClient(tb testing.TB, cfg *config.Config) (*Client, *task.Group) {
	tb.Helper()
	root := task.NewRoot(tb.Context(), "test", zerolog.Nop())
	tb.Cleanup(func() {
		root.Close()
		<-root.Done()
	})
	c, err := NewClient(root.Child("nntp"), cfg, zerolog.Nop())
	if err != nil {
		tb.Fatal(err)
	}
	return c, root
}

func stopLifecycle(tb testing.TB, root *task.Group) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(tb.Context()), lifecycleStopBudget)
	defer cancel()
	if err := root.Stop(ctx); err != nil {
		tb.Fatalf("stop: %v", err)
	}
}

func activeCount(pp *ProviderPool) int {
	n := 0
	pp.activeConns.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// TestStopDuringSlowDial: a dial parked in the greeting read must have ended,
// and its socket be closed, before the client's stop returns.
func TestStopDuringSlowDial(t *testing.T) {
	t.Parallel()
	srv := startGreetingServer(t, "")
	c, root := startLifecycleClient(t, lifecycleConfig(srv.port))
	dialDone := make(chan struct{})
	go func() {
		defer close(dialDone)
		_, _, _ = c.getConnectionFromProvider(context.Background(), WorkloadBackground, c.providers[0])
	}()
	<-srv.accepted
	stopLifecycle(t, root)
	select {
	case <-dialDone:
	default:
		t.Fatal("dial still running when stop returned")
	}
	select {
	case <-srv.sawClose:
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw the dial's socket close")
	}
	if n := activeCount(c.orderedPools[0]); n != 0 {
		t.Fatalf("%d active connections after stop", n)
	}
}

// TestLateDialStoreAfterStop: a dial that finishes its handshake and is then
// overtaken by the stop, between its pre-check and its registration, must
// close the connection itself and report the stop.
func TestLateDialStoreAfterStop(t *testing.T) {
	t.Parallel()
	srv := startGreetingServer(t, "200 ready\r\n")
	c, root := startLifecycleClient(t, lifecycleConfig(srv.port))
	c.afterDialStoreHook = func() {
		c.g.Close()
		<-c.g.Context().Done()
	}
	conn, _, err := c.getConnectionFromProvider(t.Context(), WorkloadBackground, c.providers[0])
	if conn != nil {
		t.Fatal("dial handed out a connection after the stop")
	}
	if !errors.Is(err, task.ErrStopped) {
		t.Fatalf("err = %v, want task.ErrStopped", err)
	}
	if n := activeCount(c.orderedPools[0]); n != 0 {
		t.Fatalf("%d active connections registered after stop", n)
	}
	select {
	case <-srv.sawClose:
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw the late connection close")
	}
	stopLifecycle(t, root)
}

// TestStopDuringKeepalivePing: a reaper sweep pinging a connection whose
// server never answers must have ended, and closed the connection, before the
// client's stop returns.
func TestStopDuringKeepalivePing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c, root := startLifecycleClient(t, lifecycleConfig(1))
		pp := c.orderedPools[0]
		conn := newSilentPipeConnection(t)
		pp.mu.Lock()
		poolEntry(pp, conn, 40*time.Second)
		pp.mu.Unlock()

		<-time.After(reaperInterval)
		synctest.Wait() // the sweep is inside the ping now
		stopLifecycle(t, root)
		if !conn.IsClosed() {
			t.Fatal("pinged connection still open when stop returned")
		}
	})
}

// TestClientStopLeavesNoTasks: after the stop returns, the client's tree has
// no task left.
func TestClientStopLeavesNoTasks(t *testing.T) {
	t.Parallel()
	_, root := startLifecycleClient(t, lifecycleConfig(1))
	if len(root.Snapshot()) == 0 {
		t.Fatal("a running client owns no tasks")
	}
	stopLifecycle(t, root)
	if left := root.Snapshot(); len(left) != 0 {
		t.Fatalf("tasks left after stop: %v", left)
	}
}

// TestAcquireAfterStop: a stopped client refuses new work with the stop error.
func TestAcquireAfterStop(t *testing.T) {
	t.Parallel()
	c, root := startLifecycleClient(t, lifecycleConfig(1))
	stopLifecycle(t, root)
	if _, _, err := c.Stat(t.Context(), WorkloadBackground, "<a@b>"); !errors.Is(err, task.ErrStopped) {
		t.Fatalf("Stat err = %v, want task.ErrStopped", err)
	}
	if _, err := c.BatchStat(t.Context(), []string{"<a@b>"}); !errors.Is(err, task.ErrStopped) {
		t.Fatalf("BatchStat err = %v, want task.ErrStopped", err)
	}
}

// TestCallerCancelDuringVerifyKeepsPool: three stale idle connections, the
// caller cancels while the first verify ping is in flight. The ping is bounded
// by the client alone, so the cancel costs at most that one connection.
func TestCallerCancelDuringVerifyKeepsPool(t *testing.T) {
	t.Parallel()
	c, _ := startLifecycleClient(t, lifecycleConfig(1))
	pp := c.orderedPools[0]

	clientSide, serverSide := net.Pipe()
	top := &Connection{
		conn:   clientSide,
		reader: bufio.NewReader(clientSide),
		writer: bufio.NewWriter(clientSide),
	}
	t.Cleanup(func() { _ = top.Close(); _ = serverSide.Close() })
	pinged := make(chan struct{})
	release := make(chan struct{})
	go func() {
		defer serverSide.Close()
		if _, err := bufio.NewReader(serverSide).ReadString('\n'); err != nil {
			return
		}
		close(pinged)
		<-release // the connection dies once the caller is gone
	}()

	older := []*Connection{newPipeConnection(t, true), newPipeConnection(t, true)}
	stale := 2 * c.staleThreshold
	pp.mu.Lock()
	poolEntry(pp, older[0], stale)
	poolEntry(pp, older[1], stale)
	poolEntry(pp, top, stale)
	pp.mu.Unlock()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, _, err := c.getConnectionFromProvider(ctx, WorkloadBackground, c.providers[0])
		errc <- err
	}()
	<-pinged
	cancel()
	close(release)
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	closed := 0
	for _, conn := range append(older, top) {
		if conn.IsClosed() {
			closed++
		}
	}
	if closed > 1 {
		t.Fatalf("%d connections closed, want at most 1", closed)
	}
	pp.mu.Lock()
	idle := len(pp.conns)
	pp.mu.Unlock()
	if idle != 2 {
		t.Fatalf("%d idle connections left, want 2", idle)
	}
}

// newTestGroup returns a client group under a test root. The cleanup stops it
// and waits, so a test never leaves a client task behind.
func newTestGroup(tb testing.TB) *task.Group {
	tb.Helper()
	root := task.NewRoot(tb.Context(), "test", zerolog.Nop())
	tb.Cleanup(func() {
		root.Close()
		<-root.Done()
	})
	return root.Child("nntp")
}

// newTestClient finishes a Client built from a literal: it gets a group and a
// janitor on it, as NewClient would give it. It starts no reaper or closer.
func newTestClient(tb testing.TB, c *Client) *Client {
	tb.Helper()
	c.g = newTestGroup(tb)
	c.janitor = newBodyJanitor(c.g)
	return c
}
