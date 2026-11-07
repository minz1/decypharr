package manager

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/repair"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// filesRecovery binds every file of one entry and keeps the jobs it queues active, like the real service, which
// returns the job already under way for a file instead of creating another.
type filesRecovery struct {
	fakeArrRecovery

	entryID string
	files   map[string]bool

	jobsMu sync.Mutex
	active []reacquire.Job
}

func (f *filesRecovery) Lookup(entryID, fileID string) (reacquire.Binding, bool) {
	if entryID != f.entryID || !f.files[fileID] {
		return reacquire.Binding{}, false
	}
	return reacquire.Binding{EntryID: entryID, EntryFileID: fileID}, true
}

func (f *filesRecovery) Reacquire(request reacquire.Request) (*reacquire.Job, error) {
	f.jobsMu.Lock()
	defer f.jobsMu.Unlock()
	for _, job := range f.active {
		if job.EntryID == request.EntryID && job.FileID == request.FileID {
			return &job, nil
		}
	}
	f.calls.Add(1)
	job := reacquire.Job{
		ID: fmt.Sprintf("job-%d", len(f.active)), EntryID: request.EntryID, FileID: request.FileID,
		Status: reacquire.StatusQueued,
	}
	f.active = append(f.active, job)
	return &job, nil
}

func (f *filesRecovery) Jobs() []reacquire.Job {
	f.jobsMu.Lock()
	defer f.jobsMu.Unlock()
	return append([]reacquire.Job(nil), f.active...)
}

func droppedEntryWithFiles(count int) (*storage.Entry, *filesRecovery) {
	entry := &storage.Entry{
		InfoHash: droppedHash,
		Name:     "Show.S01",
		Category: "sonarr",
		Files:    map[string]*storage.File{},
	}
	recovery := &filesRecovery{entryID: droppedHash, files: map[string]bool{}}
	for i := range count {
		id := fmt.Sprintf("file-%02d", i)
		entry.Files[fmt.Sprintf("e%02d.mkv", i)] = &storage.File{ID: id, Name: id}
		recovery.files[id] = true
	}
	return entry, recovery
}

// The dropped path retries on every sync until the entry is recovered. A retry of files that already have a job
// asks nothing of the breaker, so retries cannot use up its caps.
func TestDroppedRetriesDoNotUseUpTheHealCaps(t *testing.T) {
	t.Parallel()
	entry, recovery := droppedEntryWithFiles(10)
	m, _ := newDroppedTestManager(t, recovery)
	m.repair = repair.New(repair.Dependencies{Storage: m.storage})

	for range 4 {
		if err := m.recoverDroppedEntry("realdebrid", entry); err != nil {
			t.Fatal(err)
		}
	}
	if recovery.calls.Load() != 10 {
		t.Fatalf("reacquire calls = %d, want one per file", recovery.calls.Load())
	}
	// The 10 files used 10 of the bucket's 25; the retries must have used nothing.
	for i := range 15 {
		err := m.repair.Admit(t.Context(), repair.HealRequest{Provider: "realdebrid", Class: repair.ClassDropped})
		if err != nil {
			t.Fatalf("heal %d of the remaining capacity refused: %v", i, err)
		}
	}
	err := m.repair.Admit(t.Context(), repair.HealRequest{Provider: "realdebrid", Class: repair.ClassDropped})
	if !errors.Is(err, repair.ErrHealCapped) {
		t.Fatalf("26th heal = %v, want the cap", err)
	}
}

// An entry of 26 files is admitted once, for the files still to do, not file by file.
func TestDroppedEntryIsAdmittedOnceForAllItsFiles(t *testing.T) {
	t.Parallel()
	entry, recovery := droppedEntryWithFiles(26)
	m, _ := newDroppedTestManager(t, recovery)
	m.repair = repair.New(repair.Dependencies{Storage: m.storage})

	if err := m.recoverDroppedEntry("realdebrid", entry); err != nil {
		t.Fatalf("a 26-file entry was refused partway: %v", err)
	}
	if recovery.calls.Load() != 26 {
		t.Fatalf("reacquire calls = %d, want all 26 files", recovery.calls.Load())
	}
}

func TestStreamReacquireOfAFileWithAJobIsNotAHeal(t *testing.T) {
	t.Parallel()
	recovery := &filesRecovery{entryID: "nzb-1", files: map[string]bool{"file-1": true}}
	recovery.active = []reacquire.Job{{ID: "job-0", EntryID: "nzb-1", FileID: "file-1", Status: reacquire.StatusQueued}}
	m := &Manager{checkStreamFile: confirmBroken}
	m.SetArrRecovery(recovery)
	m.repair = repair.New(repair.Dependencies{})
	tripBucket(t, m.repair, m.repair.ProviderFor("nzb-1"), repair.ClassUsenetSegment)

	if err := m.admitStreamHeal(recovery, "nzb-1", "file-1"); err != nil {
		t.Fatalf("a file with a job under way was held: %v", err)
	}
	if err := m.admitStreamHeal(recovery, "nzb-1", "file-2"); !errors.Is(err, repair.ErrHealHeld) {
		t.Fatalf("a file without a job = %v, want held by the tripped bucket", err)
	}
}
