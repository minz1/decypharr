package rclone

import (
	"testing"
	"testing/synctest"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

// onExit runs after rcd has left Ready: a reader in it (or racing it) never sees the dead rcd as Ready.
func TestOnExitSeesNotReady(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newRcdHarness(t)
		var seen rcdSnapshot
		h.owner.onExit = func() { seen = h.owner.snapshot() }
		h.pingOK.Store(true)
		h.start()
		h.wantState(rcdReady)
		if got := h.owner.snapshot(); got.state != rcdReady || got.gen != 1 {
			t.Fatalf("snapshot after the first Ready = %+v, want Ready gen 1", got)
		}

		h.proc(0).exit()
		synctest.Wait()
		if seen.state == rcdReady {
			t.Fatalf("onExit saw %+v, want a state that is not Ready", seen)
		}
		h.advance(rcdBackoffBase)
		if got := h.owner.snapshot(); got.state != rcdReady || got.gen != 2 {
			t.Fatalf("snapshot after the restart = %+v, want Ready gen 2", got)
		}
		h.wantStopped()
	})
}

func TestFirstReadyMounts(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		r := newManagerRig(t)
		r.startMounted()
		if got := r.m.owner.snapshot(); got.state != rcdReady || got.gen != 1 {
			t.Fatalf("snapshot = %+v, want Ready gen 1", got)
		}
		r.stop()
	})
}

func TestSupClassAndCoalesce(t *testing.T) {
	t.Parallel()
	commands := map[supKind]bool{supMountDone: true, supProbeDone: true, supRetryDue: true, supProbeDue: true,
		supWatchDown: true, supBackendStopped: true, supRcdChanged: false, supTableChanged: false}
	for k := supRcdChanged; k <= supBackendStopped; k++ {
		want := keyed.Observation
		if commands[k] {
			want = keyed.Command
		}
		if got := supClass(supMsg{kind: k}); got != want {
			t.Errorf("supClass(%d) = %v, want %v", k, got, want)
		}
	}
	if a, b := supCoalesce(supMsg{kind: supTableChanged}), supCoalesce(supMsg{kind: supRcdChanged}); a == b {
		t.Fatalf("table and rcd observations share coalesce key %q", a)
	}
}
