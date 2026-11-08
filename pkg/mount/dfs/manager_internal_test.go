package dfs

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/state"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/vfs"
)

// Start sets vfs before the owner moves to Ready, so Stats must read it only
// once the state is Ready; the VFS counters would also have reported ready: true.
func TestDFSStatsBeforeReadyHasNoVFS(t *testing.T) {
	t.Parallel()
	m := &Manager{vfs: &vfs.Manager{}, state: state.NewAtomic(dfsStarting, dfsSuccessors)}

	stats := m.Stats()

	if stats["ready"] != false {
		t.Fatalf("ready = %v, want false before Start finished", stats["ready"])
	}
	if stats["state"] != "starting" {
		t.Fatalf("state = %v, want starting", stats["state"])
	}
	if _, ok := stats["total_files"]; ok {
		t.Fatal("stats carry VFS counters before the manager was ready")
	}

	if !m.move(dfsStarting, dfsReady) {
		t.Fatal("Starting to Ready refused")
	}
	if _, ok := m.Stats()["total_files"]; !ok {
		t.Fatal("ready manager lacks the VFS counters")
	}
}

// Refresh, CleanupCache and PurgeCache need a ready mount; a failed one has a
// closed cache.
func TestDFSNeedsReady(t *testing.T) {
	t.Parallel()
	m := &Manager{state: state.NewAtomic(dfsFailed, dfsSuccessors)}
	if err := m.Refresh([]string{"a"}); err == nil {
		t.Error("Refresh on a failed mount succeeded")
	}
	if _, err := m.CleanupCache(); err == nil {
		t.Error("CleanupCache on a failed mount succeeded")
	}
	if _, err := m.PurgeCache(); err == nil {
		t.Error("PurgeCache on a failed mount succeeded")
	}
}
