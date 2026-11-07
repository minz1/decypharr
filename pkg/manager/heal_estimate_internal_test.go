package manager

import (
	"testing"

	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/repair"
)

// A retry asks the breaker only for the files that need a new job. Files that already have one are not counted, not
// even for the moment between the admission and the end of the sync.
func TestDroppedRetryAsksOnlyForTheFilesWithoutAJob(t *testing.T) {
	t.Parallel()
	entry, recovery := droppedEntryWithFiles(10)
	for i := range 6 { // a first sync created these
		id := "file-0" + string(rune('0'+i))
		recovery.active = append(recovery.active, reacquire.Job{
			ID: "old-" + id, EntryID: entry.InfoHash, FileID: id, Status: reacquire.StatusQueued,
		})
	}
	m, _ := newDroppedTestManager(t, recovery)
	m.repair = repair.New(repair.Dependencies{Storage: m.storage})

	duringTheSync := -1
	recovery.afterAdmit = func() {
		if duringTheSync >= 0 {
			return
		}
		// What the breaker holds right now: its capacity, measured by taking it all and giving it back.
		left := capacityLeft(t, m.repair)
		m.repair.Refund(repair.HealRequest{Provider: "realdebrid", Class: repair.ClassDropped, Units: left})
		duringTheSync = left
	}
	if err := m.recoverDroppedEntry("realdebrid", entry); err != nil {
		t.Fatal(err)
	}
	if recovery.calls.Load() != 4 {
		t.Fatalf("jobs created = %d, want the 4 files without one", recovery.calls.Load())
	}
	if duringTheSync != 25-4 {
		t.Fatalf("capacity left during the sync = %d, want %d: only the 4 files needing a job are asked for",
			duringTheSync, 25-4)
	}
	if left := capacityLeft(t, m.repair); left != 25-4 {
		t.Fatalf("capacity left afterwards = %d, want %d", left, 25-4)
	}
}
