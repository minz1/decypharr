package reader

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/buffer"
	"github.com/sirrobot01/decypharr/internal/task"
)

const poolStopBudget = 30 * time.Second

// newTestPools builds Pools on a root group that the test stops when it ends.
// Caches the test opens close first (their cleanups are registered later).
func newTestPools(t testing.TB, memoryBudget int64) (*Pools, *task.Group) {
	t.Helper()
	g := task.NewRoot(t.Context(), "test", zerolog.Nop())
	t.Cleanup(func() { stopTestGroup(t, g) })
	pools, err := NewPools(g, memoryBudget)
	if err != nil {
		t.Fatalf("NewPools: %v", err)
	}
	return pools, g
}

// newTestSegmentCache opens a cache on cfg. Without Pools it gets its own,
// sized as the cache's memory window, on a group the test stops.
func newTestSegmentCache(
	t testing.TB,
	segments []SegmentMeta,
	cfg Config,
	stats *Stats,
	log zerolog.Logger,
) (*SegmentCache, error) {
	t.Helper()
	if cfg.Pools == nil {
		cfg.Pools, _ = newTestPools(t, memoryWindowSize(cfg, segments))
	}
	return NewSegmentCache(context.WithoutCancel(t.Context()), segments, cfg, stats, log)
}

func stopTestGroup(t testing.TB, g *task.Group) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), poolStopBudget)
	defer cancel()
	if err := g.Stop(ctx); err != nil {
		t.Errorf("stop the test group: %v", err)
	}
}

func TestPoolBudgetsBelongToEachRun(t *testing.T) {
	t.Parallel()
	var previous *Pools
	for _, budget := range []int64{64 << 20, 8 << 20} {
		pools, g := newTestPools(t, budget)
		if pools == previous || pools.buffers.Stats().MemoryBudget != budget ||
			pools.extents.stats().MemoryBudget != budget {
			t.Fatalf(
				"new run retained old budget: buffers=%#v extents=%#v",
				pools.buffers.Stats(),
				pools.extents.stats(),
			)
		}
		cfg := DefaultConfig()
		cfg.Pools = pools
		for _, retention := range []Retention{RetentionWindow, RetentionRewind} {
			cfg.Retention = retention
			checkCacheUsesSharedPools(t, cfg)
		}
		stopTestGroup(t, g)
		if pools.extents.stats().Caches != 0 || pools.buffers.Stats().Buffers != 0 {
			t.Fatal("closed run retains caches")
		}
		_, err := NewSegmentCache(t.Context(), mkSegs(4, 1024), cfg, &Stats{}, zerolog.Nop())
		if !errors.Is(err, buffer.ErrClosed) {
			t.Fatalf("old run accepted a cache: %v", err)
		}
		previous = pools
	}
}

// checkCacheUsesSharedPools opens and closes a cache on cfg.Pools.
func checkCacheUsesSharedPools(t *testing.T, cfg Config) {
	t.Helper()
	cache, err := NewSegmentCache(t.Context(), mkSegs(4, 1024), cfg, &Stats{}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if cache.pools != cfg.Pools {
		t.Fatal("cache did not use shared service pools")
	}
	if closeErr := cache.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestCacheWithoutPoolsRefused(t *testing.T) {
	t.Parallel()
	_, err := NewSegmentCache(t.Context(), mkSegs(4, 1024), DefaultConfig(), &Stats{}, zerolog.Nop())
	if !errors.Is(err, errPoolsRequired) {
		t.Fatalf("cache without pools: %v, want errPoolsRequired", err)
	}
}

// newClientGroup returns a group for an nntp client under a root the test
// stops when it ends. It is registered before the test's pools and caches, so
// the client stops after them.
func newClientGroup(t testing.TB) *task.Group {
	t.Helper()
	root := task.NewRoot(t.Context(), "test-nntp", zerolog.Nop())
	t.Cleanup(func() { stopTestGroup(t, root) })
	return root.Child("nntp")
}
