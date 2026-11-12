package rclone

import (
	"net/http"
	"testing"
	"testing/synctest"
)

// A watched mount that goes Degraded is probed at the fast cadence, not the watchdog's.
func TestWatchedDegradedProbesFast(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.watchOn()
		r.startMounted()

		r.failRC("operations/list", http.StatusBadRequest)
		r.after(watchdogInterval)
		r.wantMount(mountDegraded)
		r.wantCount("rc:mount/listmounts", 1)
		r.after(probeInterval)
		r.wantCount("rc:mount/listmounts", 2)
		r.stop()
	})
}

// wantBackendDegraded fails unless the mount is Degraded for the stopped WebDAV server and was mounted once.
func (r *managerRig) wantBackendDegraded() {
	r.t.Helper()
	r.wantMount(mountDegraded)
	if got := r.m.view.Load().err; got != "WebDAV server stopped" {
		r.t.Fatalf("mount error = %q, want %q", got, "WebDAV server stopped")
	}
	r.wantCount("rc:mount/mount", 1)
}

// A mount that vanished after the WebDAV server stopped is never remounted against the stopped server.
func TestBackendStoppedMountGoneNoRemount(t *testing.T) {
	t.Parallel()
	t.Run("probes", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newManagerRig(t)
			r.startMounted()
			r.closeBackend()
			r.mounted.Store(false)

			r.after(probeInterval)
			r.after(probeInterval)
			r.wantBackendDegraded()
			r.stop()
		})
	})
	t.Run("table detach", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newManagerRig(t)
			r.watchOn()
			r.startMounted()
			r.closeBackend()

			r.unplug()
			r.tableChanged()
			r.wantBackendDegraded()
			r.stop()
		})
	})
	t.Run("failed probe ok", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := newManagerRig(t)
			r.failRound()
			r.closeBackend()
			r.failRC("mount/mount", 0)

			r.after(failedBackoffBase)
			r.wantMount(mountFailed)
			r.wantCount("rc:mount/mount", mountAttempts)
			if got := r.m.view.Load().err; got != "WebDAV server stopped" {
				t.Fatalf("mount error = %q, want %q", got, "WebDAV server stopped")
			}
			r.stop()
		})
	})
}
