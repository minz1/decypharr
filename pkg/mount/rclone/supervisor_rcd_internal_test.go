package rclone

import (
	"testing"
	"testing/synctest"

	"github.com/sirrobot01/decypharr/internal/config"
)

// muteExit drops the rcd exit report, as if it were coalesced away behind the next Ready.
func (r *managerRig) muteExit() { r.m.owner.onExit = func() {} }

// A restart whose exit report never arrived (coalesced with the next Ready) still moves the mount to the new rcd.
func TestRcdRestartCoalescedRemounts(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.muteExit()
		r.startMounted()

		r.proc().exit()
		synctest.Wait()
		r.after(rcdBackoffBase)
		r.wantState(rcdReady)
		r.wantMount(mountMounted)
		r.wantCount("rc:mount/mount", 2)
		r.stop()
	})
}

// A table detach handled after rcd left Ready, but before the rcd report, mounts nothing against the dead rcd.
func TestTableChangeAfterRcdLeftReadyDoesNotMount(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.watchOn()
		r.muteExit()
		r.startMounted()

		r.proc().exit()
		synctest.Wait()
		r.wantState(rcdRestarting)
		r.unplug()
		r.tableChanged()
		r.wantMount(mountWaiting)
		r.wantCount("rc:mount/mount", 1)

		r.after(rcdBackoffBase)
		r.wantMount(mountMounted)
		r.wantCount("rc:mount/mount", 2)
		r.stop()
	})
}

// Every spawn trigger other than the rcd report checks the rcd first; a probe that would remount finds it gone.
func TestSpawnGuardOnEveryTrigger(t *testing.T) {
	t.Parallel()
	t.Run("degraded probe remount", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newManagerRig(t)
			r.muteExit()
			r.startMounted()
			r.mounted.Store(false)
			r.after(probeInterval)
			r.wantMount(mountDegraded)

			r.holdRcdDown()
			r.after(degradedProbeInterval)
			r.wantMount(mountWaiting)
			r.wantCount("rc:mount/mount", 1)
			r.stop()
		})
	})
	t.Run("retry due", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newManagerRig(t)
			r.muteExit()
			r.failRC("mount/mount", 400)
			r.start()
			r.wantMount(mountMounting)

			r.holdRcdDown()
			r.after(config.DefaultRetryDelay)
			r.wantMount(mountWaiting)
			r.wantCount("rc:mount/mount", 1)
			r.stop()
		})
	})
	t.Run("failed probe ok", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newManagerRig(t)
			r.muteExit()
			r.failRound()
			r.failRC("mount/mount", 0)

			r.holdRcdDown()
			r.after(failedBackoffBase)
			r.wantMount(mountWaiting)
			r.wantCount("rc:mount/mount", mountAttempts)
			r.stop()
		})
	})
}

// holdRcdDown kills rcd and keeps every later one from answering a ping, while the RC endpoint still answers
// probes: rcd stays out of Ready, and a probe that would remount finds it gone.
func (r *managerRig) holdRcdDown() {
	r.pingHeld.Store(true)
	r.proc().exit()
	synctest.Wait()
	r.wantState(rcdRestarting)
}
