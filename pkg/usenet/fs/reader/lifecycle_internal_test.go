package reader

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/internal/task"
)

// stallingBodyServer accepts connections, answers the handshake and DATE
// probes, reports each BODY command on arrived and never answers it.
type stallingBodyServer struct {
	port    int
	arrived chan string
}

func startStallingBodyServer(t *testing.T) *stallingBodyServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &stallingBodyServer{
		port:    listener.Addr().(*net.TCPAddr).Port,
		arrived: make(chan string, 8),
	}
	var (
		mu      sync.Mutex
		conns   []net.Conn
		serving sync.WaitGroup
	)
	serving.Go(func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			serving.Go(func() { srv.serve(conn) })
		}
	})
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		for _, conn := range conns {
			_ = conn.Close()
		}
		mu.Unlock()
		serving.Wait()
	})
	return srv
}

func (s *stallingBodyServer) serve(conn net.Conn) {
	defer conn.Close()
	if _, err := io.WriteString(conn, "200 stalling body test\r\n"); err != nil {
		return
	}
	lines := bufio.NewReader(conn)
	for {
		line, err := readSkippingDate(conn, lines)
		if err != nil {
			return
		}
		select {
		case s.arrived <- line:
		default:
		}
	}
}

// stopTestReader stops sr and waits for its cache to release, bounded by
// poolStopBudget.
func stopTestReader(t testing.TB, sr *StreamingReader) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), poolStopBudget)
	defer cancel()
	return sr.Stop(ctx)
}

// lifecycleStack is a reader rig over one Pools group the test controls.
type lifecycleStack struct {
	pools    *Pools
	g        *task.Group
	client   *nntp.Client
	sched    *FetchScheduler
	segments []SegmentMeta
}

func newLifecycleStack(t *testing.T, port int, budget int64, segments []SegmentMeta) *lifecycleStack {
	t.Helper()
	client, err := nntp.NewClient(newClientGroup(t), &config.Config{Usenet: config.Usenet{
		Providers: []config.UsenetProvider{{Host: "127.0.0.1", Port: port, MaxConnections: 2}},
	}}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	pools, g := newTestPools(t, budget)
	sched, _, _ := newTestScheduler(t, newTestRoot(t), 2)
	return &lifecycleStack{pools: pools, g: g, client: client, sched: sched, segments: segments}
}

func (s *lifecycleStack) open() (*StreamingReader, error) {
	return NewStreamingReader(s.g.Context(), s.client, s.segments,
		WithPools(s.pools), WithFetchScheduler(s.sched), WithPrefetchAhead(0), WithMaxConnections(2))
}

// stopAndCheckEmpty stops the stack's group and requires no task left.
func (s *lifecycleStack) stopAndCheckEmpty(t *testing.T) {
	t.Helper()
	stopTestGroup(t, s.g)
	if left := s.g.Snapshot(); len(left) != 0 {
		t.Fatalf("tasks left after stop: %+v", left)
	}
}

func TestPoolsStopWithInflightFetch(t *testing.T) {
	t.Parallel()
	srv := startStallingBodyServer(t)
	st := newLifecycleStack(t, srv.port, 64<<20, mkSegs(2, 1000))
	sr, err := st.open()
	if err != nil {
		t.Fatal(err)
	}

	readDone := make(chan error, 1)
	go func() {
		_, readErr := sr.ReadAt(make([]byte, 16), 0)
		readDone <- readErr
	}()
	<-srv.arrived

	stopTestGroup(t, st.g)
	if readErr := <-readDone; readErr == nil {
		t.Fatal("a read in flight at stop returned no error")
	}
	if state := sr.cache.GetState(0); state == StateOnDisk {
		t.Fatalf("segment 0 published after stop: %v", state)
	}
	if extents := st.pools.extents.stats(); extents.MemoryInUse != 0 || extents.Caches != 0 {
		t.Fatalf("extent pool after stop: %+v", extents)
	}
	if left := st.g.Snapshot(); len(left) != 0 {
		t.Fatalf("tasks left after stop: %+v", left)
	}
}

func TestNewStreamingReaderAfterPoolsStopped(t *testing.T) {
	t.Parallel()
	srv := startStallingBodyServer(t)
	st := newLifecycleStack(t, srv.port, 64<<20, mkSegs(2, 1000))
	st.stopAndCheckEmpty(t)

	if sr, err := st.open(); !errors.Is(err, task.ErrStopped) || sr != nil {
		t.Fatalf("open after stop: reader=%t err=%v; want task.ErrStopped", sr != nil, err)
	}
	if extents := st.pools.extents.stats(); extents.MemoryInUse != 0 || extents.Caches != 0 {
		t.Fatalf("a refused reader allocated extents: %+v", extents)
	}
	if stats := st.pools.buffers.Stats(); stats.MemoryInUse != 0 || stats.Buffers != 0 {
		t.Fatalf("a refused reader allocated buffers: %+v", stats)
	}
}

func TestReadAfterCloseIsClosedPipe(t *testing.T) {
	t.Parallel()
	srv := startStallingBodyServer(t)
	st := newLifecycleStack(t, srv.port, 64<<20, mkSegs(2, 1000))
	sr, err := st.open()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := sr.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if _, readErr := sr.ReadAt(make([]byte, 8), 0); !errors.Is(readErr, io.ErrClosedPipe) {
		t.Fatalf("ReadAt after Close = %v, want io.ErrClosedPipe", readErr)
	}
	if _, readErr := sr.Read(make([]byte, 8)); !errors.Is(readErr, io.ErrClosedPipe) {
		t.Fatalf("Read after Close = %v, want io.ErrClosedPipe", readErr)
	}
	st.stopAndCheckEmpty(t)
}

func TestReopenAfterAsyncCloseAdmits(t *testing.T) {
	t.Parallel()
	_, client, all := ppStack(t, 1, 2, 0)
	// One segment of budget: the second reader is admitted only once the
	// first one's extents are released.
	pools, g := newTestPools(t, ppSegSize)
	sched, _, _ := newTestScheduler(t, newTestRoot(t), 2)
	open := func() (*StreamingReader, error) {
		return NewStreamingReader(g.Context(), client, all[0],
			WithPools(pools), WithFetchScheduler(sched), WithPrefetchAhead(0), WithMaxConnections(2))
	}
	buf := make([]byte, 64)

	first, err := open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = first.ReadAt(buf, 0); err != nil {
		t.Fatalf("first reader: %v", err)
	}
	if closeErr := first.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	second, err := open()
	if err != nil {
		t.Fatalf("reopen right after Close: %v", err)
	}
	if n, readErr := second.ReadAt(buf, 0); readErr != nil || n != len(buf) {
		t.Fatalf("first read of the reopened reader = %d, %v", n, readErr)
	}
	stopTestGroup(t, g)
	if extents := pools.extents.stats(); extents.MemoryInUse != 0 || extents.Caches != 0 {
		t.Fatalf("extent pool after stop: %+v", extents)
	}
	if left := g.Snapshot(); len(left) != 0 {
		t.Fatalf("tasks left after stop: %+v", left)
	}
}
