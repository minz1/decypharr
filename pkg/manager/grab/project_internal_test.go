package grab

import "testing"

func TestProjectGolden(t *testing.T) {
	t.Parallel()
	const p = 0.4
	want := map[Phase]Projection{
		PhaseAbsent: {},
		PhaseQueued: {
			Client:    true,
			Dashboard: true,
			QBitState: "queuedDL",
			SABStatus: "Queued",
			SABFile:   "queued",
			Label:     "Queued",
			Progress:  0,
		},
		PhaseThrottled: {
			Client:    true,
			Dashboard: true,
			QBitState: "queuedDL",
			SABStatus: "Queued",
			SABFile:   "queued",
			Label:     "Waiting",
			Progress:  0,
		},
		PhaseSubmitting: {
			Client:    true,
			Dashboard: true,
			QBitState: "queuedDL",
			SABStatus: "Queued",
			SABFile:   "queued",
			Label:     "Submitting",
			Progress:  0,
		},
		PhaseProvider: {
			Client:    true,
			Dashboard: true,
			QBitState: "downloading",
			SABStatus: "Downloading",
			SABFile:   "active",
			Label:     "Downloading",
			Progress:  p,
		},
		PhaseAwaitingLinks: {
			Client:    true,
			Dashboard: true,
			QBitState: "downloading",
			SABStatus: "Downloading",
			SABFile:   "active",
			Label:     "Waiting for links",
			Progress:  1,
		},
		PhaseActing: {
			Client:    true,
			Dashboard: true,
			QBitState: "downloading",
			SABStatus: "Downloading",
			SABFile:   "active",
			Label:     "Linking",
			Progress:  p,
		},
		PhaseComplete: {
			Client:    true,
			Dashboard: true,
			QBitState: "pausedUP",
			SABStatus: "Completed",
			SABFile:   "finished",
			Label:     "Completed",
			Progress:  1,
		},
		PhaseFailed: {
			Client:    true,
			Dashboard: true,
			QBitState: "error",
			SABStatus: "Failed",
			SABFile:   "failed",
			Label:     "Failed",
			Progress:  p,
		},
		PhaseDeleting: {Dashboard: true, Label: "Deleting"},
	}
	for _, ph := range AllPhases() {
		if got := Project(State{Phase: ph}, p); got != want[ph] {
			t.Errorf("%v: Project = %+v, want %+v", ph, got, want[ph])
		}
	}
	if got := Project(State{Phase: PhaseActing}, 0).Progress; got != 1 {
		t.Errorf("acting at 0: progress = %v, want 1", got)
	}
	if got := Project(State{Phase: PhaseProvider}, 0.4).Progress; got != 0.4 {
		t.Errorf("provider at 0.4: progress = %v, want 0.4", got)
	}
	if got := Project(State{Phase: PhaseFailed}, 0.4).Progress; got != 0.4 {
		t.Errorf("failed at 0.4: progress = %v, want 0.4", got)
	}
}

func TestProjectExhaustive(t *testing.T) {
	t.Parallel()
	labels := map[string]Phase{}
	for _, ph := range AllPhases() {
		p := Project(State{Phase: ph}, 0.4)
		if ph == PhaseAbsent {
			if p.Dashboard || p.Client {
				t.Errorf("absent is listed: %+v", p)
			}
			continue
		}
		if !p.Dashboard || p.Label == "" {
			t.Errorf("%v: dashboard %v label %q", ph, p.Dashboard, p.Label)
		}
		if other, dup := labels[p.Label]; dup {
			t.Errorf("%v and %v share the label %q", other, ph, p.Label)
		}
		labels[p.Label] = ph
		if ph == PhaseDeleting {
			if p.Client {
				t.Errorf("deleting is listed to clients")
			}
			continue
		}
		if !p.Client || p.QBitState == "" || p.SABStatus == "" || p.SABFile == "" {
			t.Errorf("%v: incomplete projection %+v", ph, p)
		}
	}
}

func TestActingNeverProjectsComplete(t *testing.T) {
	t.Parallel()
	states := []State{{Phase: PhaseActing}, {Phase: PhaseAwaitingLinks}, {Phase: PhaseActing, PackKey: "pack"}}
	for _, st := range states {
		for _, progress := range []float64{0, 0.5, 1} {
			p := Project(st, progress)
			if p.QBitState == "pausedUP" || p.SABStatus == "Completed" || p.SABFile == "finished" {
				t.Errorf("%v pack=%q at %v projects complete: %+v", st.Phase, st.PackKey, progress, p)
			}
		}
	}
}
