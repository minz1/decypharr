package rclone

import (
	"testing"
	"testing/synctest"
)

// stackedRig is mounted on top of a stale FUSE mount at the same path, with the mount table watched.
func stackedRig(t *testing.T) *managerRig {
	t.Helper()
	r := newManagerRig(t)
	r.watchOn()
	r.lower = []tableEntry{{id: 5, parent: 1}}
	r.startMounted()
	return r
}

// A stale mount below ours going away is not a detach: our mount ID is still in the table.
func TestStaleLowerDetachKeepsMount(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := stackedRig(t)
		r.mu.Lock()
		r.lower, r.table = nil, r.table[1:]
		r.mu.Unlock()

		r.tableChanged()
		r.wantMount(mountMounted)
		r.wantCount("rc:mount/mount", 1)
		r.stop()
	})
}

// Our mount going away while a stale one stays below is a detach, though the path still shows a FUSE mount.
func TestOurDetachRemounts(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := stackedRig(t)
		r.mu.Lock()
		r.table = r.table[:1]
		r.mu.Unlock()
		r.mounted.Store(false)

		r.tableChanged()
		r.wantMount(mountMounted)
		r.wantCount("rc:mount/mount", 2)
		r.stop()
	})
}
