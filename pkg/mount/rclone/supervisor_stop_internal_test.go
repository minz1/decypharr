package rclone

import (
	"fmt"
	"net/http"
	"os/exec"
	"testing"
	"testing/synctest"
	"time"
)

// lastProc is the newest rcd process.
func (r *managerRig) lastProc() *fakeProc {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.procs[len(r.procs)-1]
}

// detachCount is how many forced unmounts ran.
func (r *managerRig) detachCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.detaches)
}

// The stop lands while a mount attempt is in flight and failing: the attempt's
// retry must never run, before or after the stop's unmount.
func TestStopDuringMountAttemptNoLateMount(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		gate := make(chan struct{})
		r.mountGate = gate
		r.failRC("mount/mount", http.StatusBadRequest)
		r.start()
		r.wantMount(mountMounting)

		done := make(chan error, 1)
		go func() { done <- r.g.Stop(t.Context()) }()
		close(gate)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		synctest.Wait()

		r.wantCount("rc:mount/mount", 1)
		last := r.lastIndex("rc:mount/mount")
		if unmounted := r.index("rc:mount/unmount"); unmounted >= 0 && last > unmounted {
			t.Fatalf("mount at %d after the unmount at %d; events: %v", last, unmounted, r.events)
		}
		r.wantMount(mountUnmounted)
		r.wantState(rcdStopped)
	})
}

func TestStopFromMountedUsesRC(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.startMounted()
		r.stop()
		r.wantCount("rc:mount/unmount", 1)
		if n := r.detachCount(); n != 0 {
			t.Fatalf("forced unmounts = %d, want 0; events: %v", n, r.events)
		}
		r.wantMount(mountUnmounted)
	})
}

// A Failed mount may still be held by rcd: the stop asks rcd to drop it.
func TestStopFromFailedUsesRC(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.failRound()
		r.wantState(rcdReady)
		before := r.detachCount()

		r.stop()
		r.wantCount("rc:mount/unmount", 1)
		if n := r.detachCount(); n != before {
			t.Fatalf("forced unmounts during the stop = %d, want 0; events: %v", n-before, r.events)
		}
		r.wantMount(mountUnmounted)
		if info := r.m.mountInfo(); info.Error == "" {
			t.Fatalf("the failure was lost on the way out: %+v", info)
		}
	})
}

func TestStopAfterRcdDiedForces(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.startMounted()
		r.proc().exit()
		synctest.Wait()
		r.wantState(rcdRestarting)
		r.wantMount(mountWaiting)

		r.stop()
		r.wantCount("rc:mount/unmount", 0)
		r.wantLiveDetach()
		r.wantMount(mountUnmounted)
	})
}

func TestStopNeverAttemptedSkipsUnmount(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.spawnErr = fmt.Errorf("start rclone: %w", exec.ErrNotFound)
		r.start()
		r.wantState(rcdFailed)

		r.stop()
		r.wantCount("rc:mount/unmount", 0)
		if n := r.detachCount(); n != 0 {
			t.Fatalf("forced unmounts = %d, want 0", n)
		}
		r.wantMount(mountUnmounted)
		if info := r.m.mountInfo(); info != nil {
			t.Fatalf("mountInfo = %+v, want nil: nothing was attempted", info)
		}
	})
}

func TestStatsMountsNullBeforeAttempt(t *testing.T) {
	t.Parallel()
	r := newManagerRig(t)
	info, ok := r.m.Stats()["mounts"].(*MountInfo)
	if !ok || info != nil {
		t.Fatalf("stats mounts = %#v, want a nil *MountInfo", r.m.Stats()["mounts"])
	}
}

// Probe failures, lost mounts and rcd exits race the stop: every move stays
// legal, the stop ends in Unmounted and leaves no task behind.
func TestSupervisorStress(t *testing.T) {
	t.Parallel()
	for i := range 50 {
		synctest.Test(t, func(t *testing.T) {
			r := newManagerRig(t)
			r.startMounted()
			switch i % 4 {
			case 1:
				r.failRC("operations/list", http.StatusBadRequest)
			case 2:
				r.mounted.Store(false)
			case 3:
				r.failRC("mount/listmounts", http.StatusBadRequest)
			default:
			}
			r.after(probeInterval + time.Duration(i%7)*time.Second)

			done := make(chan error, 1)
			go func() { done <- r.g.Stop(t.Context()) }()
			if i%2 == 0 {
				r.lastProc().exit()
			}
			if err := <-done; err != nil {
				t.Fatalf("iteration %d: %v", i, err)
			}
			synctest.Wait()

			r.wantMount(mountUnmounted)
			if n := r.m.mountRefused.Load(); n != 0 {
				t.Fatalf("iteration %d: %d refused moves; events: %v", i, n, r.events)
			}
			if left := r.g.Snapshot(); len(left) != 0 {
				t.Fatalf("iteration %d: tasks left: %v", i, left)
			}
		})
	}
}
