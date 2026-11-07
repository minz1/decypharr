package rclone

import (
	"net/http"
	"testing"
	"testing/synctest"
)

// A remount that is mounting holds the recovery guard, so a failing health
// check meanwhile starts no second recovery that would unmount under it.
func TestRemountHoldsRecoveryGuard(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.mountGate = make(chan struct{})
		r.failRC("operations/list", http.StatusBadRequest)
		r.m.info.Store(&MountInfo{LocalPath: "/mnt/x", Mounted: false, Error: "earlier"})
		r.start()
		if got := r.count("rc:mount/mount"); got != 1 {
			t.Fatalf("mount calls = %d, want the remount held open", got)
		}

		r.m.performMountHealthCheck(t.Context())
		synctest.Wait()
		if got := r.count("rc:mount/unmount"); got != 0 {
			t.Fatalf("a recovery unmounted under the remount: %d unmount calls", got)
		}

		close(r.mountGate)
		synctest.Wait()
		if got := r.count("rc:mount/mount"); got != 1 {
			t.Fatalf("mount calls = %d, want 1", got)
		}
		if r.m.recovering.Load() {
			t.Fatal("the guard is still held after the remount")
		}
		if err := r.m.Stop(); err != nil {
			t.Fatal(err)
		}
	})
}

// With no mount yet there is nothing to check, so no RC call and no warning.
func TestHealthCheckSkipsWithoutMount(t *testing.T) {
	t.Parallel()
	r := newManagerRig(t)
	r.m.owner.transition(rcdNew, rcdStarting)
	r.m.owner.transition(rcdStarting, rcdReady)

	r.m.performMountHealthCheck(t.Context())

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) != 0 {
		t.Fatalf("RC calls with no mount: %v", r.events)
	}
}
