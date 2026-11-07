package reacquire

import (
	"encoding/json"
	"testing"
	"time"
)

// fileDeleteOf returns the job's one journaled file delete.
func fileDeleteOf(t *testing.T, job Job) FileDelete {
	t.Helper()
	if len(job.FileDeletes) != 1 || job.FileDeletes[0].ArrFileID != fakeSonarrFileID {
		t.Fatalf("file deletes = %+v, want one for file %d", job.FileDeletes, fakeSonarrFileID)
	}
	return job.FileDeletes[0]
}

func requireMonitored(t *testing.T, fake *fakeSonarr, episodeIDs ...int) {
	t.Helper()
	for _, id := range episodeIDs {
		if !fake.isMonitored(id) {
			t.Errorf("episode %d is not monitored", id)
		}
	}
}

// seedFileDelete journals a delete the way an earlier run left it.
func seedFileDelete(t *testing.T, rig *fakeSonarrRig, record FileDelete) {
	t.Helper()
	if _, err := rig.service.updateJobDurable(rig.job.ID, StatusQueued, func(job *Job) {
		job.FileDeletes = []FileDelete{record}
	}); err != nil {
		t.Fatal(err)
	}
}

// A DELETE whose answer was lost may have unmonitored the episodes. The
// episodes recorded before it are monitored again once the file is found gone,
// and the job is never failed on the way.
func TestUnknownFileDeleteRestoresTheRecordedEpisodes(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101, 102)
	fake.setDeleteMode("drop-landed")
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101, 102)

	queued := rig.run(t)
	if queued.Status != StatusQueued {
		t.Fatalf("after an unknown delete the job is %s: %s", queued.Status, queued.LastError)
	}
	record := fileDeleteOf(t, queued)
	if !record.ConfirmedAt.IsZero() || record.AttemptedAt.IsZero() || len(record.EpisodeIDs) != 2 {
		t.Fatalf("journaled delete = %+v, want an attempted, unconfirmed delete of both episodes", record)
	}
	if fake.isMonitored(101) {
		t.Fatal("the fake did not unmonitor the episodes: the test proves nothing")
	}

	waiting := rig.run(t)
	if waiting.Status != StatusWaitingForGrab {
		t.Fatalf("job = %s: %s", waiting.Status, waiting.LastError)
	}
	requireMonitored(t, fake, 101, 102)
	record = fileDeleteOf(t, waiting)
	if record.ConfirmedAt.IsZero() || !record.Remonitored {
		t.Fatalf("journaled delete = %+v, want confirmed and remonitored", record)
	}
	if deletes, searches := fake.deleteCount(), fake.searchCount(); deletes != 1 || searches != 1 {
		t.Fatalf("deletes = %d, searches = %d, want 1 and 1", deletes, searches)
	}
}

// The journal survives a restart between the DELETE and the next attempt.
func TestFileDeleteJournalSurvivesARestart(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	fake.setDeleteMode("drop-landed")
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)
	if queued := rig.run(t); queued.Status != StatusQueued {
		t.Fatalf("job = %s: %s", queued.Status, queued.LastError)
	}
	if err := rig.service.Close(); err != nil {
		t.Fatal(err)
	}
	rig.open(t)

	waiting := rig.run(t)
	if waiting.Status != StatusWaitingForGrab {
		t.Fatalf("job = %s: %s", waiting.Status, waiting.LastError)
	}
	requireMonitored(t, fake, 101)
	if deletes := fake.deleteCount(); deletes != 1 {
		t.Fatalf("deletes = %d, want 1", deletes)
	}
}

// A crash after the intent was written and before the DELETE was sent: the
// next run sends it, once.
func TestFileDeleteIntentWithoutADeleteIsSentOnce(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101, 102)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101, 102)
	seedFileDelete(t, rig, FileDelete{
		ArrFileID: fakeSonarrFileID, EpisodeIDs: []int{101, 102}, AttemptedAt: time.Now().UTC(), Attempts: 1,
	})

	waiting := rig.run(t)
	if waiting.Status != StatusWaitingForGrab {
		t.Fatalf("job = %s: %s", waiting.Status, waiting.LastError)
	}
	if deletes := fake.deleteCount(); deletes != 1 {
		t.Fatalf("deletes = %d, want 1", deletes)
	}
	requireMonitored(t, fake, 101, 102)
}

// A crash after the DELETE landed and before it was confirmed: the next run
// finds the file gone, sends no second DELETE and puts the episodes back.
func TestFileDeleteThatLandedIsNotSentAgain(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101, 102)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101, 102)
	seedFileDelete(t, rig, FileDelete{
		ArrFileID: fakeSonarrFileID, EpisodeIDs: []int{101, 102}, AttemptedAt: time.Now().UTC(), Attempts: 1,
	})
	fake.loseFile()

	waiting := rig.run(t)
	if waiting.Status != StatusWaitingForGrab {
		t.Fatalf("job = %s: %s", waiting.Status, waiting.LastError)
	}
	if deletes, searches := fake.deleteCount(), fake.searchCount(); deletes != 0 || searches != 1 {
		t.Fatalf("deletes = %d, searches = %d, want 0 and 1", deletes, searches)
	}
	requireMonitored(t, fake, 101, 102)
	if record := fileDeleteOf(t, waiting); record.ConfirmedAt.IsZero() || !record.Remonitored {
		t.Fatalf("journaled delete = %+v, want confirmed and remonitored", record)
	}
}

// Re-monitoring is idempotent and best effort: a refusal does not stop the
// search the delete made necessary, and the next run tries again.
func TestFailedRemonitorDoesNotStopTheSearch(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	fake.setMonitorFailing(true)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)

	waiting := rig.run(t)
	if waiting.Status != StatusWaitingForGrab || fake.searchCount() != 1 {
		t.Fatalf("job = %s, searches = %d: %s", waiting.Status, fake.searchCount(), waiting.LastError)
	}
	if record := fileDeleteOf(t, waiting); record.ConfirmedAt.IsZero() || record.Remonitored {
		t.Fatalf("journaled delete = %+v, want confirmed and not remonitored", record)
	}
}

// A delete that was sent and refused leaves the job for an operator: failing
// it would drop the search the Arr still owes.
func TestRefusedFileDeleteNeverFailsTheJob(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	fake.setDeleteMode("refuse")
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)

	stopped := rig.run(t)
	if stopped.Status != StatusNeedsAttention {
		t.Fatalf("job = %s: %s", stopped.Status, stopped.LastError)
	}
	if record := fileDeleteOf(t, stopped); !record.ConfirmedAt.IsZero() {
		t.Fatalf("journaled delete = %+v, want unconfirmed", record)
	}
}

// A DELETE that never lands is sent a bounded number of times.
func TestLostFileDeleteIsSentAtMostThreeTimes(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	fake.setDeleteMode("drop-lost")
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)

	for attempt := 1; attempt <= maxMutationAttempts; attempt++ {
		if queued := rig.run(t); queued.Status != StatusQueued {
			t.Fatalf("attempt %d: job = %s: %s", attempt, queued.Status, queued.LastError)
		}
	}
	stopped := rig.run(t)
	if stopped.Status != StatusNeedsAttention {
		t.Fatalf("job = %s: %s", stopped.Status, stopped.LastError)
	}
	if deletes := fake.deleteCount(); deletes != maxMutationAttempts {
		t.Fatalf("deletes = %d, want %d", deletes, maxMutationAttempts)
	}
}

func TestUnconfirmedFileDeleteCountsAsAnAttemptedIntent(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC()
	settled := Mutation{
		Key: "movie_search:7", Kind: MutationMovieSearch, State: MutationConfirmed, CommandName: "MoviesSearch",
		MovieIDs: []int{7}, IntentAt: base, LastDispatchedAt: base, ConfirmedAt: base, Attempts: 1,
	}
	attempted := FileDelete{ArrFileID: 5, AttemptedAt: base.Add(-10 * time.Minute), Attempts: 3}

	job := Job{StartedAt: base, Mutations: []Mutation{settled}}
	if deadline := job.reconciliationDeadline(); !deadline.IsZero() {
		t.Fatalf("a job with nothing unconfirmed has deadline %v", deadline)
	}
	job.FileDeletes = []FileDelete{attempted}
	if got, want := job.reconciliationDeadline(), attempted.AttemptedAt.Add(reconciliationTimeout); !got.Equal(want) {
		t.Fatalf("deadline = %v, want %v from the first attempted delete", got, want)
	}
	if got := retryDelay(job, nil); got != 4*retryBaseDelay {
		t.Fatalf("retry delay = %v, want %v after three attempts", got, 4*retryBaseDelay)
	}

	attempted.ConfirmedAt = base
	job.FileDeletes = []FileDelete{attempted}
	if deadline := job.reconciliationDeadline(); !deadline.IsZero() {
		t.Fatalf("a confirmed delete still sets deadline %v", deadline)
	}
	if got := retryDelay(job, nil); got != retryBaseDelay {
		t.Fatalf("a confirmed delete still backs the retry off to %v", got)
	}
}

// The previous binary decodes a stored job with plain [json.Unmarshal] and
// validateJob, neither of which looks at fields it does not know. The
// journal is a field of its own, and a job without deletes is stored as before.
func TestFileDeleteJournalIsAnOptionalJobField(t *testing.T) {
	t.Parallel()
	job := legacyJob("journaled")
	job.Strategy = StrategyCommandSearch
	plain, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if unmarshalErr := json.Unmarshal(plain, &fields); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if _, present := fields["fileDeletes"]; present {
		t.Fatalf("a job without deletes stores fileDeletes: %s", plain)
	}

	job.FileDeletes = []FileDelete{{ArrFileID: 5, EpisodeIDs: []int{1}, AttemptedAt: time.Now().UTC(), Attempts: 1}}
	journaled, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var previous struct {
		ID        string     `json:"id"`
		Status    Status     `json:"status"`
		Strategy  Strategy   `json:"strategy"`
		Mutations []Mutation `json:"mutations"`
	}
	if unmarshalErr := json.Unmarshal(journaled, &previous); unmarshalErr != nil {
		t.Fatalf("a reader without the field fails: %v", unmarshalErr)
	}
	if previous.ID != "journaled" || previous.Strategy != StrategyCommandSearch {
		t.Fatalf("a reader without the field reads %+v", previous)
	}

	var reread Job
	if unmarshalErr := json.Unmarshal(journaled, &reread); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if validateErr := validateJob(reread); validateErr != nil {
		t.Fatalf("validateJob() = %v", validateErr)
	}
	if len(reread.FileDeletes) != 1 {
		t.Fatalf("round trip lost the journal: %+v", reread.FileDeletes)
	}
}
