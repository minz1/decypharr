package manager

import (
	"context"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet"
)

func TestAddNewNZBQueuesBeforeNetworkParsing(t *testing.T) {
	t.Parallel()
	usenetClient := newUnreachableUsenet(t)
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		m := withTestConfig(t, h.manager())
		m.usenet = usenetClient

		req := NewNZBRequest(
			"release.nzb",
			t.TempDir(),
			[]byte("network parsing must happen in the worker"),
			arr.Arr{Name: "sonarr"},
			config.DownloadActionSymlink,
			"",
			ImportTypeSABnzbd,
			false,
		)
		id, err := m.AddNewNZB(t.Context(), req)
		if err != nil {
			t.Fatalf("AddNewNZB() error = %v", err)
		}
		if id != req.ID {
			t.Fatalf("AddNewNZB() id = %q, want %q", id, req.ID)
		}
		if len(req.NZBContent) != 0 {
			t.Fatal("queued request retained NZB content in memory")
		}
		run := h.next(grab.ChildSubmit)
		if run.call.grab.State.Phase != grab.PhaseSubmitting || !run.call.grab.State.NZB {
			t.Fatalf("submit child started in %+v, want an NZB submitting", run.call.grab.State)
		}
		entry, err := store.GetQueuedGrab(id)
		if err != nil {
			t.Fatalf("get queued NZB: %v", err)
		}
		if entry.Status != storage.EntryStatusQueued || entry.Magnet == "" {
			t.Fatalf("queued NZB status/path = %q/%q", entry.Status, entry.Magnet)
		}
		if _, statErr := os.Stat(entry.Magnet); statErr != nil {
			t.Fatalf("staged NZB source: %v", statErr)
		}
	})
}

// newUnreachableUsenet builds a usenet client whose only provider refuses
// connections, so any network parse fails fast.
func newUnreachableUsenet(t *testing.T) *usenet.Usenet {
	t.Helper()
	store := testConfigStore(t, func(cfg *config.Config) {
		cfg.Usenet = config.Usenet{
			Providers: []config.UsenetProvider{{
				Host:           "127.0.0.1",
				Port:           1,
				MaxConnections: 1,
			}},
			MaxConnections:           1,
			ProcessingMaxConnections: 1,
		}
	})

	usenetRoot := task.NewRoot(t.Context(), "test", zerolog.Nop())
	t.Cleanup(func() { stopUsenetRoot(t, usenetRoot) })
	usenetClient, err := usenet.New(usenetRoot, store, logger.Discard())
	if err != nil {
		t.Fatalf("create usenet client: %v", err)
	}
	return usenetClient
}

// stopUsenetRoot stops the group a test hung its usenet service on.
func stopUsenetRoot(t *testing.T, root *task.Group) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
	defer cancel()
	if err := root.Stop(ctx); err != nil {
		t.Errorf("stop the usenet group: %v", err)
	}
}
