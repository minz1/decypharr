package rclone

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

// watchOn, before start, gives the manager a fake mount table watcher and a fake table: mount/mount shows in the
// table as soon as it is asked for, mount/unmount removes it.
func (r *managerRig) watchOn() {
	r.watching = true
	r.changedCh = make(chan struct{})
	r.failCh = make(chan error, 1)
	r.m.watchTable = func(ctx context.Context, changed func()) error {
		r.mu.Lock()
		r.changed = changed
		r.mu.Unlock()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-r.changedCh:
				changed()
			case err := <-r.failCh:
				return err
			}
		}
	}
	r.m.readTable = func(string) ([]tableEntry, error) {
		r.mu.Lock()
		gate := r.readGate
		r.readGate = nil
		r.mu.Unlock()
		if gate != nil {
			<-gate
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		return slices.Clone(r.table), r.tableErr
	}
}

// tableChanged makes the fake watcher report a mount table change.
func (r *managerRig) tableChanged() {
	r.changedCh <- struct{}{}
	synctest.Wait()
}

// failWatch ends the fake watcher with an error.
func (r *managerRig) failWatch() {
	r.failCh <- errors.New("watch failed")
	synctest.Wait()
}

// currentMountGate is the gate mount/mount waits on, if any.
func (r *managerRig) currentMountGate() chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mountGate
}

// gateMounts makes every later mount/mount wait until the returned channel is closed.
func (r *managerRig) gateMounts() chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mountGate = make(chan struct{})
	return r.mountGate
}

// gateRead makes the next readTable call, and so the supervisor, wait until the returned channel is closed.
func (r *managerRig) gateRead() chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.readGate = make(chan struct{})
	return r.readGate
}

func (r *managerRig) setTableErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tableErr = err
}

// callChanged calls the callback the fake watcher was given, as a watcher still running would.
func (r *managerRig) callChanged() {
	r.mu.Lock()
	changed := r.changed
	r.mu.Unlock()
	changed()
}

// clearTable empties the fake table: nothing is mounted at our path.
func (r *managerRig) clearTable() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.table = nil
}

// newEntryLocked hands out a fresh mount ID on top of the parent; r.mu must be held.
func (r *managerRig) newEntryLocked(parent int) tableEntry {
	r.nextID++
	return tableEntry{id: 100 + r.nextID, parent: parent}
}

// plugMount is a mount/mount reaching the kernel: the table shows a fresh mount on top of the lower ones (unless
// tableIgnoresMount) and rclone lists it.
func (r *managerRig) plugMount() {
	r.mu.Lock()
	if !r.tableIgnoresMount {
		parent := 1
		if len(r.lower) > 0 {
			parent = r.lower[len(r.lower)-1].id
		}
		r.table = append(slices.Clone(r.lower), r.newEntryLocked(parent))
	}
	r.mu.Unlock()
	r.mounted.Store(true)
}

// unplug detaches the mount behind rclone's back: the table no longer shows it and rclone stops listing it.
func (r *managerRig) unplug() {
	r.mu.Lock()
	r.saved, r.table = r.table, nil
	r.mu.Unlock()
	r.mounted.Store(false)
}

// plug puts a detached mount back, from outside: the same mount, so the same mount ID.
func (r *managerRig) plug() {
	r.mu.Lock()
	r.table = r.saved
	r.mu.Unlock()
	r.mounted.Store(true)
}

func TestNextProbe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state   mountState
		gone    int
		watchUp bool
		want    time.Duration
	}{
		{mountMounted, 0, true, watchdogInterval},
		{mountDegraded, 0, true, probeInterval},
		{mountDegraded, 1, true, degradedProbeInterval},
		{mountMounted, 0, false, probeInterval},
		{mountDegraded, 1, false, degradedProbeInterval},
		{mountDegraded, 0, false, probeInterval},
	}
	for _, tc := range cases {
		if got := nextProbe(tc.state, tc.gone, tc.watchUp); got != tc.want {
			t.Errorf("nextProbe(%s, %d, %v) = %s, want %s", tc.state, tc.gone, tc.watchUp, got, tc.want)
		}
	}
	if watchdogInterval != 5*time.Minute {
		t.Fatalf("watchdogInterval = %s, want 5m", watchdogInterval)
	}
}

func TestWatchdogProbesEveryFiveMinutes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.watchOn()
		r.startMounted()

		r.after(watchdogInterval - time.Second)
		r.wantCount("rc:mount/listmounts", 0)
		r.after(time.Second)
		r.wantCount("rc:mount/listmounts", 1)
		r.wantMount(mountMounted)
		r.stop()
	})
}

// Q4: with the table watched, a mountGone probe while the table still shows the mount is checked again at the
// degraded cadence, and the second one remounts.
func TestWatchdogMountGoneTwiceRecovers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.watchOn()
		r.startMounted()

		r.mounted.Store(false)
		r.after(watchdogInterval)
		r.wantMount(mountDegraded)
		r.after(degradedProbeInterval - time.Second)
		r.wantMount(mountDegraded)
		r.wantCount("rc:mount/mount", 1)
		r.after(time.Second)
		r.wantMount(mountMounted)
		r.wantCount("rc:mount/mount", 2)
		r.stop()
	})
}

func TestWatchDownFallsBackToFastCadence(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.watchOn()
		r.startMounted()

		r.failWatch()
		r.after(probeInterval)
		r.wantCount("rc:mount/listmounts", 1)
		r.stop()
	})
}

func TestWatchDownWhileDegradedRechecksFast(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.watchOn()
		r.startMounted()

		r.mounted.Store(false)
		r.after(watchdogInterval)
		r.wantMount(mountDegraded)
		r.wantCount("rc:mount/listmounts", 1)

		r.failWatch()
		r.after(degradedProbeInterval)
		r.wantCount("rc:mount/listmounts", 2)
		r.wantMount(mountMounted)
		r.stop()
	})
}

// wantRemountedAfterCleanup fails unless the last mount/mount came after an unmount.
func (r *managerRig) wantRemountedAfterCleanup() {
	r.t.Helper()
	unmounted, mounted := r.lastIndex("rc:mount/unmount"), r.lastIndex("rc:mount/mount")
	if unmounted < 0 || unmounted > mounted {
		r.t.Fatalf("unmount at %d, mount at %d, want the unmount first; events: %v", unmounted, mounted, r.events)
	}
}

// Proof 1: a detach in the table recovers at once, with no probe.
func TestTableDetachRecoversWithoutProbe(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.watchOn()
		r.startMounted()

		r.unplug()
		r.tableChanged()
		r.wantMount(mountMounted)
		r.wantCount("rc:mount/mount", 2)
		r.wantRemountedAfterCleanup()
		r.wantCount("rc:mount/listmounts", 0)
		r.stop()
	})
}

// Proof 2: another mount changing the table leaves ours alone.
func TestTableChangeWithMountPresentDoesNothing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.watchOn()
		r.startMounted()
		before := r.eventCount()

		r.tableChanged()
		r.wantMount(mountMounted)
		if r.eventCount() != before {
			t.Fatalf("events after an unrelated change: %v", r.events)
		}
		r.stop()
	})
}

// Proof 3: a change delivered after the detach it reported was repaired reads the table again and does nothing.
func TestLateTableChangeAfterRepairDoesNothing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.watchOn()
		r.startMounted()

		gate := r.gateRead()
		r.tableChanged() // the supervisor now waits inside its table read
		r.unplug()
		r.tableChanged() // queued behind the busy supervisor
		r.plug()
		close(gate)
		synctest.Wait()

		r.wantMount(mountMounted)
		r.wantCount("rc:mount/mount", 1)
		r.wantCount("rc:mount/unmount", 0)
		r.stop()
	})
}

// Proof 8a: a table that never shows our mount falls back to probing and never remounts from a table change.
func TestSelfCheckMissingPathFallsBack(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.watchOn()
		r.tableIgnoresMount = true
		r.startMounted()

		r.tableChanged()
		r.wantMount(mountMounted)
		r.wantCount("rc:mount/mount", 1)
		r.after(probeInterval)
		r.wantCount("rc:mount/listmounts", 1)
		r.wantMount(mountMounted)
		r.stop()
	})
}

// Proof 8b: a detach between the kernel mount and mountDone is caught by the self-check and the fast cadence.
func TestDetachBeforeMountDoneRecoversFast(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.watchOn()
		gate := r.gateMounts()
		r.start()
		r.wantMount(mountMounting)

		r.unplug()
		r.tableChanged() // ignored while Mounting
		close(gate)
		synctest.Wait()
		r.wantMount(mountMounted)

		r.after(probeInterval)
		r.wantMount(mountDegraded)
		r.after(degradedProbeInterval)
		r.wantMount(mountMounted)
		r.wantCount("rc:mount/mount", 2)
		r.stop()
	})
}

// Proof 9: a table read error falls back to probing.
func TestTableReadErrorFallsBack(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.watchOn()
		r.startMounted()

		r.setTableErr(errors.New("read failed"))
		r.tableChanged()
		r.wantMount(mountMounted)
		r.wantCount("rc:mount/mount", 1)
		r.after(probeInterval)
		r.wantCount("rc:mount/listmounts", 1)
		r.stop()
	})
}

// Proof 10: rclone unmounting and exiting on its own converges back to Mounted, cleanup first.
func TestRcloneCleanExitConverges(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.watchOn()
		r.startMounted()

		r.unplug()
		r.failRC("mount/mount", http.StatusServiceUnavailable) // the dying rcd takes no mount
		r.tableChanged()
		r.wantMount(mountMounting)
		r.proc().exit()
		synctest.Wait()
		r.wantMount(mountWaiting)

		r.failRC("mount/mount", 0)
		r.after(rcdBackoffBase)
		r.wantState(rcdReady)
		r.wantMount(mountMounted)
		r.wantRemountedAfterCleanup()
		r.stop()
	})
}

// Our own recovery's table changes start no second mount.
func TestOwnRecoveryDetachIgnored(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.watchOn()
		r.startMounted()

		gate := r.gateMounts()
		r.unplug()
		r.tableChanged()
		r.wantMount(mountMounting)
		r.tableChanged()
		r.tableChanged()
		close(gate)
		synctest.Wait()
		r.wantMount(mountMounted)
		r.wantCount("rc:mount/mount", 2)

		r.tableChanged()
		r.wantCount("rc:mount/mount", 2)
		r.stop()
	})
}

// Proof 11: Stop ends the watcher with the rest, and a late change is refused quietly.
func TestStopEndsWatcherAndBackendTasks(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.watchOn()
		r.startMounted()

		r.stop()
		synctest.Wait()
		if tasks := r.g.Snapshot(); len(tasks) != 0 {
			t.Fatalf("tasks after Stop: %+v", tasks)
		}
		r.callChanged()
		r.wantCount("rc:mount/mount", 1)
	})
}

func (r *managerRig) eventCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// closeBackend stops the fake WebDAV server.
func (r *managerRig) closeBackend() {
	close(r.backend)
	synctest.Wait()
}

// Proof 4: the WebDAV server stopping degrades the mount without a probe, and a mount that finishes after it ends
// Degraded, not Mounted.
func TestBackendStopDegradesWithoutProbe(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.watchOn()
		r.startMounted()

		r.closeBackend()
		r.wantMount(mountDegraded)
		if got := r.m.view.Load().err; got != "WebDAV server stopped" {
			t.Fatalf("mount error = %q, want %q", got, "WebDAV server stopped")
		}
		r.wantCount("rc:mount/listmounts", 0)
		r.stop()
	})
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.watchOn()
		gate := r.gateMounts()
		r.start()
		r.wantMount(mountMounting)

		r.closeBackend()
		r.wantMount(mountMounting)
		close(gate)
		synctest.Wait()
		r.wantMount(mountDegraded)
		r.stop()
	})
}

// A stopped WebDAV server keeps the mount Degraded through a probe that finds it fine.
func TestBackendStopStaysDegraded(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.watchOn()
		r.startMounted()

		r.closeBackend()
		r.after(probeInterval)
		r.wantCount("rc:mount/listmounts", 1)
		r.wantMount(mountDegraded)
		r.stop()
	})
}

// A probe that fails while the WebDAV server is stopped keeps the stopped server as the mount's error.
func TestBackendStopKeepsItsErrorThroughFailedProbes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.watchOn()
		r.startMounted()

		r.closeBackend()
		r.failRC("operations/list", http.StatusBadRequest)
		r.after(probeInterval)
		r.wantCount("rc:operations/list", 1)
		r.wantMount(mountDegraded)
		if got := r.m.view.Load().err; got != "WebDAV server stopped" {
			t.Fatalf("mount error = %q, want %q", got, "WebDAV server stopped")
		}
		r.stop()
	})
}
