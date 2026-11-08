package cgofuse

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/config"
)

// fakeHost stands in for the cgofuse host: Mount blocks the way the real one
// does, and calls Init through the filesystem once the test lets it.
type fakeHost struct {
	fs        *FS
	initGate  chan struct{} // nil: never calls Init
	mountOK   bool
	stayUp    bool // Mount blocks until Unmount
	unmounted chan struct{}
	unmounts  int
}

func (h *fakeHost) Mount(string, []string) bool {
	if h.initGate != nil {
		<-h.initGate
		h.fs.Init()
	}
	if h.stayUp {
		<-h.unmounted
	}
	return h.mountOK
}

func (h *fakeHost) Unmount() bool {
	h.unmounts++
	select {
	case <-h.unmounted:
	default:
		close(h.unmounted)
	}
	return true
}

func newTestBackend(t *testing.T, h *fakeHost) (*Backend, *task.Group) {
	t.Helper()
	g := task.NewRoot(t.Context(), "test", zerolog.Nop())
	cfg := &config.FuseConfig{MountPath: t.TempDir(), DaemonTimeout: time.Minute}
	h.unmounted = make(chan struct{})
	b := &Backend{
		config: cfg,
		logger: zerolog.Nop(),
		root:   NewFS(nil, cfg, zerolog.Nop()),
	}
	b.newHost = func(f *FS) host {
		h.fs = f
		return h
	}
	return b, g
}

func TestMountFalseBeforeInitFails(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := &fakeHost{}
		b, g := newTestBackend(t, h)
		m, err := b.Mount(t.Context(), g)
		if err == nil || m != nil {
			t.Fatalf("Mount = (%v, %v), want an error and no handle", m, err)
		}
		stopCtx, cancel := task.Detached(t.Context(), time.Second)
		defer cancel()
		if stopErr := g.Stop(stopCtx); stopErr != nil {
			t.Fatalf("Stop = %v", stopErr)
		}
	})
}

func TestReadyOnlyAfterInit(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := &fakeHost{initGate: make(chan struct{}), stayUp: true, mountOK: true}
		b, g := newTestBackend(t, h)
		type result struct {
			ok  bool
			err error
		}
		res := make(chan result, 1)
		go func() {
			m, err := b.Mount(t.Context(), g)
			res <- result{m != nil, err}
		}()
		synctest.Wait()
		select {
		case <-res:
			t.Fatal("Mount returned before Init")
		default:
		}

		close(h.initGate)
		synctest.Wait()
		r := <-res
		if r.err != nil || !r.ok {
			t.Fatalf("Mount after Init = (%v, %v)", r.ok, r.err)
		}

		stopCtx, cancel := task.Detached(t.Context(), time.Second)
		defer cancel()
		h.Unmount()
		if stopErr := g.Stop(stopCtx); stopErr != nil {
			t.Fatalf("Stop = %v", stopErr)
		}
	})
}

func TestDoneWhenHostMountReturns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := &fakeHost{initGate: make(chan struct{}), stayUp: true, mountOK: true}
		b, g := newTestBackend(t, h)
		close(h.initGate)
		m, err := b.Mount(t.Context(), g)
		if err != nil {
			t.Fatalf("Mount = %v", err)
		}
		select {
		case <-m.Done():
			t.Fatal("Done closed while the host serves")
		default:
		}
		if unmountErr := m.Unmount(); unmountErr != nil {
			t.Fatalf("Unmount = %v", unmountErr)
		}
		synctest.Wait()
		select {
		case <-m.Done():
		default:
			t.Fatal("Done not closed after the host's Mount returned")
		}
		stopCtx, cancel := task.Detached(t.Context(), time.Second)
		defer cancel()
		if stopErr := g.Stop(stopCtx); stopErr != nil {
			t.Fatalf("Stop = %v", stopErr)
		}
	})
}
