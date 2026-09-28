package config_test

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	dfsconfig "github.com/sirrobot01/decypharr/pkg/mount/dfs/config"
)

// A zero or negative cleanup interval passes config validation (it parses)
// but panics time.NewTicker in the cache's cleanup loop; Parse must keep the
// default instead.
func TestParseRejectsNonPositiveCleanupInterval(t *testing.T) {
	t.Parallel()
	want := dfsconfig.DefaultFuseConfig().CacheCleanupInterval
	for _, value := range []string{"0s", "0", "-5m"} {
		got := dfsconfig.Parse(config.DFS{CacheCleanupInterval: value}, "/mnt", 3).CacheCleanupInterval
		if got != want {
			t.Errorf("interval %q: got %s, want default %s", value, got, want)
		}
	}
	if got := dfsconfig.Parse(config.DFS{CacheCleanupInterval: "10m"}, "/mnt", 3).CacheCleanupInterval; got.Minutes() != 10 {
		t.Errorf("valid interval not applied: got %s", got)
	}
}
