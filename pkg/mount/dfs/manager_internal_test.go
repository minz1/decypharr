package dfs

import (
	"testing"

	"github.com/sirrobot01/decypharr/pkg/mount/dfs/vfs"
)

// Start writes vfs before it publishes ready, so Stats must read it only once
// ready is set; the VFS counters would also have reported ready: true.
func TestDFSStatsBeforeReadyHasNoVFS(t *testing.T) {
	t.Parallel()
	m := &Manager{vfs: &vfs.Manager{}}

	stats := m.Stats()

	if stats["ready"] != false {
		t.Fatalf("ready = %v, want false before Start finished", stats["ready"])
	}
	if _, ok := stats["total_files"]; ok {
		t.Fatal("stats carry VFS counters before the manager was ready")
	}

	m.ready.Store(true)
	if _, ok := m.Stats()["total_files"]; !ok {
		t.Fatal("ready manager lacks the VFS counters")
	}
}
