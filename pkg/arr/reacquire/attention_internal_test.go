package reacquire

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A job that stops for an operator raises one notice with what the operator
// needs to look at.
func TestJobMovingToNeedsAttentionIsAnnouncedOnce(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	fake.setDeleteMode("refuse")
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)

	stopped := rig.run(t)
	if stopped.Status != StatusNeedsAttention {
		t.Fatalf("job = %s: %s", stopped.Status, stopped.LastError)
	}
	notices := rig.announced.all()
	if len(notices) != 1 {
		t.Fatalf("notices = %+v, want exactly one", notices)
	}
	notice := notices[0]
	if notice.JobID != rig.job.ID || notice.ArrName != "sonarr" || notice.File != fakeSonarrPath {
		t.Errorf("notice identifies %+v, want job %s, arr sonarr, file %s", notice, rig.job.ID, fakeSonarrPath)
	}
	if !strings.Contains(notice.Why, "403") {
		t.Errorf("why = %q, want the refusal", notice.Why)
	}
	if !strings.Contains(notice.Remonitor, "not confirmed") {
		t.Errorf("re-monitor result = %q, want the unconfirmed delete named", notice.Remonitor)
	}

	// Stopping a job that already stopped is not a new announcement.
	if !rig.service.stopReconciliation(rig.job.ID, errors.New("again")) {
		t.Fatal("could not stop the job again")
	}
	if got := len(rig.announced.all()); got != 1 {
		t.Fatalf("notices after a second stop = %d, want 1", got)
	}
}

func TestCompensatedStopSaysWhereTheRemonitorStands(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)
	now := time.Now().UTC()
	seedFileDelete(t, rig, FileDelete{
		ArrFileID: fakeSonarrFileID, EpisodeIDs: []int{101}, AttemptedAt: now, ConfirmedAt: now,
		Attempts: 1, Remonitored: true,
	})
	rig.handler = reconciliationHandler(func(context.Context, Job, JobProgress) error {
		return errors.New("boom")
	})

	rig.run(t)
	notices := rig.announced.all()
	if len(notices) != 1 || !strings.Contains(notices[0].Why, "boom") ||
		!strings.Contains(notices[0].Remonitor, "monitored again") {
		t.Fatalf("notices = %+v", notices)
	}
}

// Moving to any other status says nothing.
func TestOtherTransitionsAreNotAnnounced(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)
	if waiting := rig.run(t); waiting.Status != StatusWaitingForGrab {
		t.Fatalf("job = %s: %s", waiting.Status, waiting.LastError)
	}

	failing := newFakeSonarr(t, 101)
	failed := newFakeSonarrRig(t, failing, StrategyCommandSearch, 101)
	failed.handler = reconciliationHandler(func(context.Context, Job, JobProgress) error {
		return errors.New("nothing was touched")
	})
	if job := failed.run(t); job.Status != StatusFailed {
		t.Fatalf("job = %s: %s", job.Status, job.LastError)
	}

	for name, rig := range map[string]*fakeSonarrRig{"waiting": rig, "failed": failed} {
		if notices := rig.announced.all(); len(notices) != 0 {
			t.Errorf("%s job announced %+v", name, notices)
		}
	}
}
