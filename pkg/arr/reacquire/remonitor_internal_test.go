package reacquire

import (
	"strings"
	"testing"
	"time"
)

// The refused re-monitor of a deleted file's episodes is a debt the job's
// record keeps: a later maintenance pass pays it, though the job went on to
// wait for its replacement.
func TestRefusedRemonitorIsRetriedByMaintenance(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	fake.setMonitorFailing(true)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)
	waiting := rig.run(t)
	if waiting.Status != StatusWaitingForGrab || fake.searchCount() != 1 {
		t.Fatalf("job = %s, searches = %d: %s", waiting.Status, fake.searchCount(), waiting.LastError)
	}
	if record := fileDeleteOf(t, waiting); record.Remonitored || record.RemonitorAttempts != 1 {
		t.Fatalf("journaled delete = %+v, want one refused re-monitor", record)
	}

	// Not yet: the backoff has not passed.
	rig.service.maintainJobs(t.Context())
	if calls := fake.monitorRequests(); calls != 1 {
		t.Fatalf("monitor requests before the backoff = %d, want 1", calls)
	}

	fake.setMonitorFailing(false)
	rig.advance(remonitorBackoff + time.Second)
	rig.service.maintainJobs(t.Context())
	paid, _ := rig.service.Job(rig.job.ID)
	if record := fileDeleteOf(t, paid); !record.Remonitored {
		t.Fatalf("journaled delete = %+v, want remonitored", record)
	}
	requireMonitored(t, fake, 101)
	if paid.Status != StatusWaitingForGrab {
		t.Fatalf("the retry changed the job to %s", paid.Status)
	}
}

// A job that never gets its episodes monitored again says so, and the service
// stops asking the arr.
func TestPersistentRemonitorFailureIsSurfaced(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101, 102)
	fake.setMonitorFailing(true)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101, 102)
	rig.run(t)

	for range 2 * remonitorMaxAttempts {
		rig.advance(2 * time.Hour)
		rig.service.maintainJobs(t.Context())
	}
	job, _ := rig.service.Job(rig.job.ID)
	if record := fileDeleteOf(t, job); record.Remonitored || record.RemonitorAttempts != remonitorMaxAttempts {
		t.Fatalf("journaled delete = %+v, want %d refused tries", record, remonitorMaxAttempts)
	}
	if !strings.Contains(job.LastError, "could not be monitored again") || !strings.Contains(job.LastError, "101") {
		t.Fatalf("last error = %q, want the unmonitored episodes named", job.LastError)
	}
	if calls := fake.monitorRequests(); calls != remonitorMaxAttempts {
		t.Fatalf("monitor requests = %d, want %d", calls, remonitorMaxAttempts)
	}
}

// A job that is about to become ready first tries the re-monitor it owes.
func TestOwedRemonitorIsTriedBeforeTheJobIsReady(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	fake.setMonitorFailing(true)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)
	if waiting := rig.run(t); waiting.Status != StatusWaitingForGrab {
		t.Fatalf("job = %s: %s", waiting.Status, waiting.LastError)
	}
	fake.setMonitorFailing(false)
	rig.advance(remonitorBackoff + time.Second)

	replacement := fakeSonarrBinding(fake.instance(), 101)
	replacement.EntryID, replacement.EntryFileID, replacement.DownloadID = "entry-2", "file-2", "download-2"
	replacement.ArrFileID, replacement.LibraryPath = fakeSonarrFileID+1, "/library/show/s01e01-new.mkv"
	if err := rig.service.UpsertBinding(replacement); err != nil {
		t.Fatal(err)
	}
	ready, _ := rig.service.Job(rig.job.ID)
	if ready.Status != StatusReady {
		t.Fatalf("job = %s: %s", ready.Status, ready.LastError)
	}
	if record := fileDeleteOf(t, ready); !record.Remonitored {
		t.Fatalf("journaled delete = %+v, want remonitored before ready", record)
	}
	requireMonitored(t, fake, 101)
}

// After a confirmed delete, the wait for a replacement that ends with nothing
// imported does not fail the job when no search was ever taken: the arr was
// left to search on its own, and did not.
func TestExpiryAfterAConfirmedDeleteWithoutASearchSearches(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	fake.letArrRedownload()
	rig := newFakeSonarrRig(t, fake, StrategyHistoryFailed, 101)
	waiting := rig.run(t)
	if waiting.Status != StatusWaitingForGrab || fake.searchCount() != 0 || confirmedSearchOrGrab(waiting) {
		t.Fatalf("job = %s, searches = %d: %s", waiting.Status, fake.searchCount(), waiting.LastError)
	}
	if err := rig.service.SetHandler(rig.handler); err != nil {
		t.Fatal(err)
	}

	rig.advance(waitingTimeout + time.Minute)
	rig.service.maintainJobs(t.Context())
	expired, _ := rig.service.Job(rig.job.ID)
	if expired.Status != StatusNeedsAttention || fake.searchCount() != 1 {
		t.Fatalf("expired job = %s, searches = %d: %s", expired.Status, fake.searchCount(), expired.LastError)
	}
}
