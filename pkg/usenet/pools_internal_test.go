package usenet

import (
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/pkg/usenet/fs/reader"
)

func TestRestartAppliesUsenetMemoryBudget(t *testing.T) {
	t.Parallel()
	loaded, err := config.Load(t.TempDir(), config.MapEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	store := config.NewStore(loaded)
	var firstDepth int
	var firstPools *reader.Pools
	for _, budget := range []string{"64MB", "8MB"} {
		if _, updateErr := store.Update(func(cfg *config.Config) error {
			cfg.Usenet.Providers = []config.UsenetProvider{{Host: "127.0.0.1", Port: 1, MaxConnections: 1}}
			cfg.Usenet.BufferMemory = budget
			return nil
		}); updateErr != nil {
			t.Fatal(updateErr)
		}
		service, newErr := New(store, logger.Discard())
		if newErr != nil {
			t.Fatal(newErr)
		}
		t.Cleanup(func() { _ = service.Close() })
		cfg := reader.DefaultConfig()
		cfg.Pools = service.bufferPools
		segments := make([]reader.SegmentMeta, 64)
		for i := range segments {
			segments[i].Bytes = 1 << 20
		}
		cache, cacheErr := reader.NewSegmentCache(t.Context(), segments, cfg, &reader.Stats{}, zerolog.Nop())
		if cacheErr != nil {
			t.Fatal(cacheErr)
		}
		t.Cleanup(func() { _ = cache.Close() })
		depth := cache.MaxPrefetchSegments()
		if firstPools == nil {
			firstPools, firstDepth = service.bufferPools, depth
		} else if firstPools == service.bufferPools || depth >= firstDepth {
			t.Fatalf("restart retained its old pool budget: prefetch %d -> %d", firstDepth, depth)
		}
		if closeErr := cache.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if closeErr := service.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
}
