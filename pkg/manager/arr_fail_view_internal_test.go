package manager

import (
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
)

type jobsRecovery struct {
	*fakeArrRecovery

	mu   sync.Mutex
	byID map[string]reacquire.Job
}

func (r *jobsRecovery) JobByID(id string) (reacquire.Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.byID[id]
	return job, ok
}

func (r *jobsRecovery) set(job reacquire.Job) {
	r.mu.Lock()
	r.byID[job.ID] = job
	r.mu.Unlock()
}

func (r *jobsRecovery) drop(id string) {
	r.mu.Lock()
	delete(r.byID, id)
	r.mu.Unlock()
}

func viewManager(t *testing.T) (*Manager, *jobsRecovery) {
	t.Helper()
	now := time.Now()
	m := &Manager{failer: newArrFailer(arrFailerDeps{now: func() time.Time { return now }, logger: zerolog.Nop()})}
	rec := &jobsRecovery{fakeArrRecovery: &fakeArrRecovery{}, byID: map[string]reacquire.Job{}}
	m.SetArrRecovery(rec)
	return m, rec
}

func viewGrab(af grab.ArrFail, at time.Time) GrabView {
	g := pendingGrab("a", "rd", at)
	g.State.ArrFail = af
	return GrabView{State: g.State, Entry: *g.Entry}
}

func TestArrFailViewPending(t *testing.T) {
	t.Parallel()
	at := time.Now()
	retry := at.Add(time.Minute)
	pending := grab.ArrFail{State: grab.ArrFailPending, Arr: "sonarr"}
	tests := []struct {
		name  string
		shape func(*failItem)
		want  ArrFailView
	}{
		{"plain", func(*failItem) {}, ArrFailView{State: "pending", Arr: "sonarr"}},
		{"waiting for the arr", func(it *failItem) { it.wait = failWaitArr },
			ArrFailView{State: "pending", Arr: "sonarr", Waiting: failWaitArrStatus}},
		{"waiting for another job", func(it *failItem) { it.wait = failWaitKey },
			ArrFailView{State: "pending", Arr: "sonarr", Waiting: failWaitKeyStatus}},
		{"error and next retry", func(it *failItem) { it.fails, it.lastErr, it.due = 1, "boom", retry },
			ArrFailView{State: "pending", Arr: "sonarr", LastError: "boom", NextRetry: retry}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, _ := viewManager(t)
			g := pendingGrab("a", "rd", at)
			m.failer.wantGrab(g)
			tc.shape(m.failer.items["a"])
			got, ok := m.ArrFailView(viewGrab(pending, at))
			if !ok || got != tc.want {
				t.Fatalf("view = %+v, %v; want %+v", got, ok, tc.want)
			}
		})
	}
}

func TestArrFailViewIgnoresAnotherOccurrence(t *testing.T) {
	t.Parallel()
	at := time.Now()
	m, _ := viewManager(t)
	m.failer.wantGrab(pendingGrab("a", "rd", at))
	it := m.failer.items["a"]
	it.wait, it.lastErr = failWaitArr, "old"
	got, ok := m.ArrFailView(viewGrab(grab.ArrFail{State: grab.ArrFailPending, Arr: "sonarr"}, at.Add(time.Hour)))
	if want := (ArrFailView{State: "pending", Arr: "sonarr"}); !ok || got != want {
		t.Fatalf("view = %+v, %v; want %+v with no status of the old occurrence", got, ok, want)
	}
}

func TestArrFailViewNoneAndSkipped(t *testing.T) {
	t.Parallel()
	m, _ := viewManager(t)
	if got, ok := m.ArrFailView(viewGrab(grab.ArrFail{}, time.Now())); ok {
		t.Fatalf("view of no intent = %+v, want absent", got)
	}
	got, ok := m.ArrFailView(viewGrab(grab.ArrFail{State: grab.ArrFailSkipped, Arr: "sonarr"}, time.Now()))
	if want := (ArrFailView{State: "skipped", Arr: "sonarr"}); !ok || got != want {
		t.Fatalf("view = %+v, %v; want %+v", got, ok, want)
	}
}

func TestArrFailViewQueuedFollowsTheRecordedJob(t *testing.T) {
	t.Parallel()
	m, rec := viewManager(t)
	queued := grab.ArrFail{State: grab.ArrFailQueued, Arr: "sonarr", JobID: "job-a"}
	at := time.Now()
	rec.set(reacquire.Job{ID: "job-a", Status: reacquire.StatusBlocklisting, CreatedAt: at.Add(-time.Hour)})
	steps := []struct {
		name  string
		shape func()
		want  string
	}{
		{"older job still active", func() {}, "blocklisting"},
		{"older job settled", func() {
			rec.set(reacquire.Job{ID: "job-a", Status: reacquire.StatusReady, CreatedAt: at.Add(-time.Hour)})
		}, "ready"},
		{"older job pruned", func() { rec.drop("job-a") }, "job pruned"},
	}
	for _, step := range steps {
		step.shape()
		got, ok := m.ArrFailView(viewGrab(queued, at))
		if want := (ArrFailView{State: "queued", Arr: "sonarr", JobStatus: step.want}); !ok || got != want {
			t.Fatalf("%s: view = %+v, %v; want %+v", step.name, got, ok, want)
		}
	}
}

func TestArrFailViewQueuedWithoutAJobShowsNoStatus(t *testing.T) {
	t.Parallel()
	m, _ := viewManager(t)
	got, ok := m.ArrFailView(viewGrab(grab.ArrFail{State: grab.ArrFailQueued, Arr: "sonarr"}, time.Now()))
	if want := (ArrFailView{State: "queued", Arr: "sonarr"}); !ok || got != want {
		t.Fatalf("view = %+v, %v; want %+v", got, ok, want)
	}
}
