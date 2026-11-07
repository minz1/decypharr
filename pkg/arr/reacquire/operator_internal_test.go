package reacquire

import (
	"context"
	"strings"
	"testing"
	"time"
)

// refusingInvalidator refuses to invalidate: check says no before anything is
// sent to the Arr, invalidate says no after the search.
type refusingInvalidator struct {
	check       error
	invalidate  error
	invalidated int
}

func (inv *refusingInvalidator) CheckInvalidation(context.Context, Job) error { return inv.check }

func (inv *refusingInvalidator) InvalidateReacquire(context.Context, Job) error {
	inv.invalidated++
	return inv.invalidate
}

// A refusal that only an operator can settle stops the job in needs_attention,
// before the Arr file is deleted when it is known by then.
func TestOperatorRefusalBeforeTheDeleteSendsNothing(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)
	inv := &refusingInvalidator{check: OperatorRequired("file b.mkv of the entry is bound only heuristically")}
	rig.handler = NewHandler(rig.arrs, inv)

	stopped := rig.run(t)
	if stopped.Status != StatusNeedsAttention || !strings.Contains(stopped.LastError, "b.mkv") {
		t.Fatalf("job = %s: %s", stopped.Status, stopped.LastError)
	}
	if deletes, searches := fake.deleteCount(), fake.searchCount(); deletes != 0 || searches != 0 {
		t.Fatalf("deletes = %d, searches = %d, want nothing sent", deletes, searches)
	}
	if inv.invalidated != 0 {
		t.Fatal("the entry was invalidated")
	}
	if notices := rig.announced.all(); len(notices) != 1 {
		t.Fatalf("notices = %+v, want one", notices)
	}
}

// A pack job that deleted Arr files in an earlier run and then meets a refusal has no search taken: the files are
// gone, so it sends its compensating search before it stops.
func TestOperatorRefusalAfterAnEarlierDeleteSendsTheCompensatingSearch(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)
	now := time.Now().UTC()
	seedFileDelete(t, rig, FileDelete{
		ArrFileID: fakeSonarrFileID, EpisodeIDs: []int{101}, AttemptedAt: now, ConfirmedAt: now,
		Attempts: 1, Remonitored: true,
	})
	fake.loseFile()
	inv := &refusingInvalidator{check: OperatorRequired("a sibling file lost its binding")}
	rig.handler = NewHandler(rig.arrs, inv)

	stopped := rig.run(t)
	if stopped.Status != StatusNeedsAttention || !strings.Contains(stopped.LastError, "sibling") {
		t.Fatalf("job = %s: %s", stopped.Status, stopped.LastError)
	}
	if searches := fake.searchCount(); searches != 1 {
		t.Fatalf("searches = %d, want the one compensating search", searches)
	}
	if !strings.Contains(stopped.LastError, "compensating search was sent") {
		t.Fatalf("last error = %q, want the compensating search named", stopped.LastError)
	}
}

// The same refusal after the search is not a failure: the job did its work and
// the entry is left for an operator.
func TestOperatorRefusalAfterTheSearchIsNotAFailure(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)
	inv := &refusingInvalidator{invalidate: OperatorRequired("an imported sibling lost its binding")}
	rig.handler = NewHandler(rig.arrs, inv)

	stopped := rig.run(t)
	if stopped.Status != StatusNeedsAttention || !strings.Contains(stopped.LastError, "sibling") {
		t.Fatalf("job = %s: %s", stopped.Status, stopped.LastError)
	}
}
