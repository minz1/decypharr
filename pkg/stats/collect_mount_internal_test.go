package stats

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

// fakeMount is a MountManager with a fixed readiness and stats.
type fakeMount struct {
	ready bool
	stats map[string]any
}

func (f *fakeMount) Start(*task.Group) error  { return nil }
func (f *fakeMount) Stop() error              { return nil }
func (f *fakeMount) Stats() map[string]any    { return f.stats }
func (f *fakeMount) IsReady() bool            { return f.ready }
func (f *fakeMount) Type() string             { return "fake" }
func (f *fakeMount) Refresh(_ []string) error { return nil }

var _ manager.MountManager = (*fakeMount)(nil)

func newCollector(t *testing.T, mount manager.MountManager) (*Collector, *config.Config) {
	t.Helper()
	mgr, store := managertest.New(t, nil)
	mgr.SetMountManager(mount)
	return &Collector{mgr: mgr}, store.Get()
}

// A mount that is not ready used to be reported as just "not ready", hiding
// that rcd had failed.
func TestCollectMountShowsStateWhenNotReady(t *testing.T) {
	t.Parallel()
	c, cfg := newCollector(t, &fakeMount{stats: map[string]any{"state": "failed", "restarts": uint64(5)}})

	got := c.collectMount(cfg)

	if got.Ready {
		t.Fatal("Ready with a mount that is not ready")
	}
	if got.State != "failed" {
		t.Fatalf("State = %q, want failed", got.State)
	}
	if got.Type != "fake" || got.Detail["restarts"] != uint64(5) {
		t.Fatalf("mount stats = %+v, want the type and the detail", got)
	}
}

func TestCollectMountReadyKeepsState(t *testing.T) {
	t.Parallel()
	c, cfg := newCollector(t, &fakeMount{ready: true, stats: map[string]any{"state": "ready"}})

	got := c.collectMount(cfg)

	if !got.Ready || got.State != "ready" {
		t.Fatalf("mount stats = %+v, want ready", got)
	}
}
