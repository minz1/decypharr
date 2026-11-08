package usenet

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const lifecycleStopBudget = 30 * time.Second

// stallingServer accepts connections, answers the handshake and DATE probes,
// reports each other command on arrived and never answers it.
type stallingServer struct {
	port    int
	arrived chan string
}

func startStallingServer(t *testing.T) *stallingServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &stallingServer{port: listener.Addr().(*net.TCPAddr).Port, arrived: make(chan string, 64)}
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

func (s *stallingServer) serve(conn net.Conn) {
	defer conn.Close()
	if _, err := io.WriteString(conn, "200 stalling test\r\n"); err != nil {
		return
	}
	lines := bufio.NewReader(conn)
	for {
		line, err := lines.ReadString('\n')
		if err != nil {
			return
		}
		if line == "DATE\r\n" {
			if _, err = io.WriteString(conn, "111 20260905220000\r\n"); err != nil {
				return
			}
			continue
		}
		select {
		case s.arrived <- line:
		default:
		}
	}
}

// newLifecycleUsenet builds the service on a root group the test stops, with
// one provider on port and maxConns scheduler workers.
func newLifecycleUsenet(t *testing.T, port, maxConns int) (*Usenet, *task.Group) {
	t.Helper()
	loaded, err := config.Load(t.TempDir(), config.MapEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	store := config.NewStore(loaded)
	if _, updateErr := store.Update(func(cfg *config.Config) error {
		cfg.Usenet.Providers = []config.UsenetProvider{{Host: "127.0.0.1", Port: port, MaxConnections: maxConns}}
		cfg.Usenet.MaxConnections = maxConns
		return nil
	}); updateErr != nil {
		t.Fatal(updateErr)
	}
	root := task.NewRoot(t.Context(), "test", zerolog.Nop())
	t.Cleanup(func() { stopLifecycleGroup(t, root) })
	u, newErr := New(root, store, logger.Discard())
	if newErr != nil {
		t.Fatal(newErr)
	}
	return u, root
}

func stopLifecycleGroup(t *testing.T, g *task.Group) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), lifecycleStopBudget)
	defer cancel()
	if err := g.Stop(ctx); err != nil {
		t.Errorf("stop the test group: %v", err)
	}
}

// addLifecycleFile stores an NZB with one plain file of segs 1000-byte segments.
func addLifecycleFile(t *testing.T, u *Usenet, id, name string, segs int) {
	t.Helper()
	file := storage.NZBFile{NzbID: id, Name: name, Size: int64(segs) * 1000}
	for i := range segs {
		off := int64(i) * 1000
		file.Segments = append(file.Segments, storage.NZBSegment{
			Number: i + 1, MessageID: fmt.Sprintf("<%s-%d@stall>", id, i), Bytes: 1000,
			StartOffset: off, EndOffset: off + 999,
		})
	}
	if err := u.nzbStorage.AddNZB(&storage.NZB{ID: id, Name: id, Files: []storage.NZBFile{file}}); err != nil {
		t.Fatal(err)
	}
}

func TestStreamAfterStopReturnsErrStopped(t *testing.T) {
	t.Parallel()
	srv := startStallingServer(t)
	u, root := newLifecycleUsenet(t, srv.port, 1)
	addLifecycleFile(t, u, "nzb", "movie.mkv", 2)
	u.analyzer = nil // VerifyFileHead then goes through createEntry, the fence under test
	stopLifecycleGroup(t, root)

	ctx := t.Context()
	if err := u.Stream(ctx, "nzb", "movie.mkv", 0, 99, io.Discard); !errors.Is(err, task.ErrStopped) {
		t.Fatalf("Stream after stop = %v, want task.ErrStopped", err)
	}
	if h, err := u.OpenFile(ctx, "nzb", "movie.mkv"); !errors.Is(err, task.ErrStopped) {
		if h != nil {
			_ = h.Close()
		}
		t.Fatalf("OpenFile after stop = %v, want task.ErrStopped", err)
	}
	file := storage.NZBFile{
		Name: "movie.mkv", Size: 2000,
		Segments: []storage.NZBSegment{{Number: 1, MessageID: "<v@stall>", Bytes: 2000, EndOffset: 1999}},
	}
	if err := u.VerifyFileHead(ctx, &file); !errors.Is(err, task.ErrStopped) {
		t.Fatalf("VerifyFileHead after stop = %v, want task.ErrStopped", err)
	}
	if n := u.fs.Size(); n != 0 {
		t.Fatalf("%d entries in the map after stop", n)
	}
	if left := root.Snapshot(); len(left) != 0 {
		t.Fatalf("tasks left after stop: %+v", left)
	}
}

// An entry already in the map must not be handed out once the work group has
// stopped: finalize is about to stop its reader.
func TestExistingEntryAfterWorkStopReturnsErrStopped(t *testing.T) {
	t.Parallel()
	srv := startStallingServer(t)
	u, _ := newLifecycleUsenet(t, srv.port, 1)
	addLifecycleFile(t, u, "nzb", "movie.mkv", 2)

	h, err := u.OpenFile(t.Context(), "nzb", "movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	stopLifecycleGroup(t, u.work)

	entry, err := u.getOrCreateEntry(t.Context(), "nzb", "movie.mkv", configuredRetention(u.streamSettings))
	if entry != nil {
		entry.releaseUsed()
	}
	if !errors.Is(err, task.ErrStopped) {
		t.Fatalf("existing entry after the work stop = %v, want task.ErrStopped", err)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
}

// finalize stops entries that streams may still hold; a handle released at the
// same moment must not race the reader fields cleanup clears.
func TestCleanupRacingReleaseIsSynchronized(t *testing.T) {
	t.Parallel()
	fe := &fsEntry{
		retention:     RetentionDelivery,
		reader:        &noPrefetchReader{},
		readerCleanup: func(context.Context) {},
	}
	fe.readerOnce.Do(func() {})
	for range 2 {
		if !fe.acquire() {
			t.Fatal("acquire a fresh entry")
		}
	}
	var wg sync.WaitGroup
	wg.Go(func() { fe.cleanup(t.Context()) })
	wg.Go(fe.releaseUsed)
	wg.Go(func() { _, _, _ = fe.getOrCreateReader() })
	wg.Wait()
	fe.releaseUsed()
}

func TestReleaseAfterRecreateKeepsReplacementRefcount(t *testing.T) {
	t.Parallel()
	srv := startStallingServer(t)
	u, _ := newLifecycleUsenet(t, srv.port, 1)
	addLifecycleFile(t, u, "nzb", "movie.mkv", 2)

	a, err := u.OpenFile(t.Context(), "nzb", "movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	u.fs.Clear()
	b, err := u.OpenFile(t.Context(), "nzb", "movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if a.entry == b.entry {
		t.Fatal("the cleared map handed the same entry back")
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if got := b.entry.refCount.Load(); got != 1 {
		t.Fatalf("replacement refcount after the old handle closed = %d, want 1", got)
	}
	if err = b.Close(); err != nil {
		t.Fatal(err)
	}
	if got := b.entry.refCount.Load(); got != 0 {
		t.Fatalf("replacement refcount after its own handle closed = %d, want 0", got)
	}
}

func TestUsenetStopUnderLoad(t *testing.T) {
	t.Parallel()
	const readers = 8
	srv := startStallingServer(t)
	u, root := newLifecycleUsenet(t, srv.port, 1)

	var reading sync.WaitGroup
	for i := range readers {
		name := fmt.Sprintf("movie-%d.mkv", i)
		addLifecycleFile(t, u, name, name, 32)
		h, err := u.OpenFile(t.Context(), name, name)
		if err != nil {
			t.Fatal(err)
		}
		h.Prefetch(t.Context(), 0, 32000)
		reading.Go(func() {
			// Every read ends with an error once the service stops.
			_, _ = h.ReadAtContext(context.WithoutCancel(t.Context()), make([]byte, 16), int64(i)*100)
			_ = h.Close()
		})
	}
	<-srv.arrived // the single worker is inside a BODY that never answers

	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Second)
	defer cancel()
	if err := root.Stop(ctx); err != nil {
		t.Fatalf("Stop under load: %v", err)
	}
	reading.Wait()
	if left := root.Snapshot(); len(left) != 0 {
		t.Fatalf("tasks left after stop: %+v", left)
	}
}

func TestUsenetStopLeavesNoTasks(t *testing.T) {
	t.Parallel()
	srv := startStallingServer(t)
	u, root := newLifecycleUsenet(t, srv.port, 2)
	addLifecycleFile(t, u, "nzb", "movie.mkv", 2)
	h, err := u.OpenFile(t.Context(), "nzb", "movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	if running := root.Snapshot(); len(running) == 0 {
		t.Fatal("a running service has no tasks")
	}
	stopLifecycleGroup(t, root)
	if left := root.Snapshot(); len(left) != 0 {
		t.Fatalf("tasks left after stop: %+v", left)
	}
	if n := u.fs.Size(); n != 0 {
		t.Fatalf("%d entries in the map after stop", n)
	}
}
