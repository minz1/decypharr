package rclone

import (
	"net/http"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
)

// after advances the bubble's clock by d and lets everything settle.
func (r *managerRig) after(d time.Duration) {
	<-time.After(d)
	synctest.Wait()
}

// wantCount fails unless event was recorded n times.
func (r *managerRig) wantCount(event string, n int) {
	r.t.Helper()
	if got := r.count(event); got != n {
		r.t.Fatalf("%s calls = %d, want %d; events: %v", event, got, n, r.events)
	}
}

// lastIndex is the index of the last event, or -1.
func (r *managerRig) lastIndex(event string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, e := range slices.Backward(r.events) {
		if e == event {
			return i
		}
	}
	return -1
}

// stop marks the stop and stops the manager, failing on an error.
func (r *managerRig) stop() {
	r.t.Helper()
	r.markStop()
	if err := r.m.Stop(); err != nil {
		r.t.Fatal(err)
	}
}

// failRound starts the rig with every mount failing and spends the first round.
func (r *managerRig) failRound() {
	r.t.Helper()
	r.failRC("mount/mount", http.StatusBadRequest)
	r.start()
	for range mountAttempts - 1 {
		r.after(config.DefaultRetryDelay)
	}
	r.wantCount("rc:mount/mount", mountAttempts)
	r.wantMount(mountFailed)
}

// startMounted starts the rig and waits for the first mount.
func (r *managerRig) startMounted() {
	r.t.Helper()
	r.start()
	r.wantMount(mountMounted)
	r.wantCount("rc:mount/mount", 1)
}

func TestFirstMountFailsThenNextRoundMounts(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.failRound()
		if info := r.m.mountInfo(); info.Error == "" || info.Mounted {
			t.Fatalf("failed mount info = %+v", info)
		}

		r.failRC("mount/mount", 0)
		r.after(failedBackoffBase)
		r.wantMount(mountMounted)
		r.wantCount("rc:mount/mount", mountAttempts+1)
		if info := r.m.mountInfo(); info.Error != "" || !info.Mounted || info.MountedAt == "" {
			t.Fatalf("mounted info = %+v", info)
		}
		r.stop()
	})
}

func TestFailedBackendDownBacksOff(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.failRound()
		r.failRC("operations/list", http.StatusBadRequest)

		r.after(failedBackoffBase)
		r.wantMount(mountFailed)
		r.wantCount("rc:operations/list", 1)
		r.wantCount("rc:mount/mount", mountAttempts)

		r.after(2*failedBackoffBase - time.Second)
		r.wantCount("rc:operations/list", 1)
		r.after(time.Second)
		r.wantCount("rc:operations/list", 2)
		r.wantMount(mountFailed)
		r.wantCount("rc:mount/mount", mountAttempts)
		r.stop()
	})
}

func TestFailedBackoffDoublesAcrossRounds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.failRound()

		// The probe finds the backend up and starts round 2, which fails too.
		r.after(failedBackoffBase)
		r.wantCount("rc:mount/listmounts", 1)
		r.wantCount("rc:mount/mount", mountAttempts+1)
		for range mountAttempts - 1 {
			r.after(config.DefaultRetryDelay)
		}
		r.wantMount(mountFailed)
		r.wantCount("rc:mount/mount", 2*mountAttempts)

		r.after(failedBackoffBase)
		r.wantCount("rc:mount/listmounts", 1)
		r.after(failedBackoffBase)
		r.wantCount("rc:mount/listmounts", 2)
		r.stop()
	})
}

func TestRcdRestartFromFailedStartsFreshRound(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.failRound()

		r.proc().exit()
		synctest.Wait()
		r.wantMount(mountWaiting)

		r.after(rcdBackoffBase)
		r.wantState(rcdReady)
		for range mountAttempts - 2 {
			r.after(config.DefaultRetryDelay)
		}
		// A fresh round: one attempt left before Failed.
		r.wantMount(mountMounting)
		r.wantCount("rc:mount/mount", 2*mountAttempts-1)
		r.after(config.DefaultRetryDelay)
		r.wantMount(mountFailed)
		r.wantCount("rc:mount/mount", 2*mountAttempts)
		r.stop()
	})
}

func TestProbeBlipDoesNotRemount(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.startMounted()

		r.failRC("operations/list", http.StatusBadRequest)
		r.after(probeInterval)
		r.wantMount(mountDegraded)
		if !r.m.IsMounted() {
			t.Fatal("a degraded mount still serves")
		}

		r.failRC("operations/list", 0)
		r.after(probeInterval)
		r.wantMount(mountMounted)
		r.wantCount("rc:mount/mount", 1)
		if info := r.m.mountInfo(); info.Error != "" {
			t.Fatalf("error kept after a good probe: %+v", info)
		}
		r.stop()
	})
}

func TestMountGoneTwiceRecovers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.startMounted()

		r.mounted.Store(false)
		r.after(probeInterval)
		r.wantMount(mountDegraded)
		r.wantCount("rc:mount/mount", 1)

		r.after(degradedProbeInterval)
		r.wantMount(mountMounted)
		r.wantCount("rc:mount/mount", 2)
		forgot, remounted := r.lastIndex("rc:mount/unmount"), r.lastIndex("rc:mount/mount")
		if forgot < 0 || forgot > remounted {
			t.Fatalf("unmount at %d, remount at %d, want the unmount first; events: %v", forgot, remounted, r.events)
		}
		r.stop()
	})
}

func TestBackendDownNeverRemounts(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.startMounted()

		r.failRC("operations/list", http.StatusBadRequest)
		for range 3 {
			r.after(probeInterval)
			r.wantMount(mountDegraded)
		}
		r.wantCount("rc:mount/mount", 1)
		r.stop()
	})
}

func TestRCErrorNeverRemounts(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.startMounted()

		r.failRC("mount/listmounts", http.StatusBadRequest)
		for range 3 {
			r.after(probeInterval)
			r.wantMount(mountDegraded)
		}
		r.wantCount("rc:mount/mount", 1)
		r.stop()
	})
}

func TestRcdDiedDropsInflightMount(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		gate := make(chan struct{})
		r.mountGate = gate
		r.start()
		r.wantMount(mountMounting)

		r.proc().exit()
		synctest.Wait()
		r.wantMount(mountWaiting)
		if info := r.m.mountInfo(); info.Error != "rclone exited" {
			t.Fatalf("info = %+v, want the exit recorded", info)
		}

		close(gate)
		synctest.Wait()
		r.wantMount(mountWaiting)

		r.after(rcdBackoffBase)
		r.wantState(rcdReady)
		r.wantMount(mountMounted)
		r.stop()
	})
}

// A mount result from before the rcd died reaches the supervisor after Mounting was entered again. The runner drops
// a cancelled child's result itself, so the stale result is sent directly: the epoch alone must drop it.
func TestStaleMountResultDroppedAfterRemount(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		gate := make(chan struct{})
		r.mountGate = gate
		r.start()
		r.wantMount(mountMounting)

		r.proc().exit()
		synctest.Wait()
		r.wantMount(mountWaiting)
		r.after(rcdBackoffBase)
		r.wantState(rcdReady)
		r.wantMount(mountMounting)
		r.wantCount("rc:mount/mount", 2)

		// Epoch 1 is the first attempt's: rcd ready, rcd died and rcd ready again moved it to 3.
		r.m.sendSupMsg(supMsg{kind: supMountDone, epoch: 1})
		synctest.Wait()
		r.wantMount(mountMounting)

		close(gate)
		synctest.Wait()
		r.wantMount(mountMounted)
		r.stop()
	})
}

// A retry and a new round each ask rclone to forget the mount first: an attempt that timed out on our side may have
// finished on rclone's.
func TestRetriesForgetTheMountFirst(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.failRC("mount/mount", http.StatusBadRequest)
		r.start()
		r.wantCount("rc:mount/unmount", 0)

		r.after(config.DefaultRetryDelay)
		r.wantCount("rc:mount/mount", 2)
		r.wantCount("rc:mount/unmount", 1)
		if forgot, retried := r.lastIndex("rc:mount/unmount"), r.lastIndex("rc:mount/mount"); forgot > retried {
			t.Fatalf("unmount at %d after the retry at %d; events: %v", forgot, retried, r.events)
		}

		for range mountAttempts - 2 {
			r.after(config.DefaultRetryDelay)
		}
		r.wantMount(mountFailed)
		before := r.count("rc:mount/unmount")

		r.failRC("mount/mount", 0)
		r.after(failedBackoffBase)
		r.wantMount(mountMounted)
		r.wantCount("rc:mount/unmount", before+1)
		if forgot, mounted := r.lastIndex("rc:mount/unmount"), r.lastIndex("rc:mount/mount"); forgot > mounted {
			t.Fatalf("unmount at %d after the new round's mount at %d; events: %v", forgot, mounted, r.events)
		}
		r.stop()
	})
}

// The first attempt after a start detaches a FUSE mount the table shows at the mount point: a killed process may have
// left a dead endpoint.
func TestFirstAttemptCleansUpStaleFUSE(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.m.readTable = func(string) (int, error) { return 1, nil }
		r.startMounted()
		detached, mounted := r.index("detach"), r.index("rc:mount/mount")
		if detached < 0 || detached > mounted {
			t.Fatalf("detach at %d, first mount at %d, want the detach first; events: %v", detached, mounted, r.events)
		}
		r.stop()
	})
}

// Without a FUSE mount at the mount point the first attempt detaches nothing: a bind or fstab mount there is the
// user's. That holds when the table shows none and when there is no table.
func TestFirstAttemptLeavesOtherMounts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{{"no FUSE entry", nil}, {"no table", errNoMountTable}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				r := newManagerRig(t)
				r.m.readTable = func(string) (int, error) { return 0, tc.err }
				r.startMounted()
				r.wantCount("detach", 0)
				r.stop()
			})
		})
	}
}

// A forget that rclone never answers is cut at its own budget: the retry still reaches mount/mount.
func TestHungForgetLeavesTheAttemptItsTime(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.failRC("mount/mount", http.StatusBadRequest)
		r.start()
		r.hangUnmount.Store(true)

		r.after(config.DefaultRetryDelay + rcdRCUnmountBudget)
		r.wantCount("rc:mount/unmount", 1)
		r.wantCount("rc:mount/mount", 2)
		r.hangUnmount.Store(false)
		r.stop()
	})
}

func TestErrorKeptUntilMounted(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.failRC("mount/mount", http.StatusBadRequest)
		r.start()

		info, ok := r.m.Stats()["mounts"].(*MountInfo)
		if !ok || info == nil || info.Error == "" || info.State != "mounting" || info.Mounted {
			t.Fatalf("stats mounts while failing = %+v", info)
		}

		r.failRC("mount/mount", 0)
		r.after(config.DefaultRetryDelay)
		info, ok = r.m.Stats()["mounts"].(*MountInfo)
		if !ok || info == nil || info.Error != "" || info.State != "mounted" || !info.Mounted {
			t.Fatalf("stats mounts once mounted = %+v", info)
		}
		r.stop()
	})
}

func TestErrorKeptThroughRecovery(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.startMounted()

		r.mounted.Store(false)
		r.failRC("mount/mount", http.StatusBadRequest)
		r.after(probeInterval)
		if info := r.m.mountInfo(); info.Error == "" || info.State != "degraded" {
			t.Fatalf("degraded info = %+v", info)
		}
		r.after(degradedProbeInterval)
		if info := r.m.mountInfo(); info.Error == "" || info.State != "mounting" || info.Mounted {
			t.Fatalf("recovering info = %+v", info)
		}

		r.failRC("mount/mount", 0)
		r.after(config.DefaultRetryDelay)
		if info := r.m.mountInfo(); info.Error != "" || info.State != "mounted" {
			t.Fatalf("recovered info = %+v", info)
		}
		r.stop()
	})
}
