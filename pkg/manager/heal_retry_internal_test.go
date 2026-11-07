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
	vetoes []error
	// failBeforeGate fails a request that has no job before the gate is asked, like an unusable binding.
	failBeforeGate error
	// racedAfterGate makes another request win the job after the gate admitted.
	racedAfterGate bool
	// afterAdmit runs after each admission, outside the fake's lock, to look at what the breaker holds meanwhile.
	afterAdmit func()
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
	if f.failBeforeGate != nil {
		return nil, f.failBeforeGate
	}
	if request.Gate != nil && request.Gate.Admit != nil {
		if err := request.Gate.Admit(); err != nil {
			f.vetoes = append(f.vetoes, err)
			return nil, err
		}
		if f.afterAdmit != nil {
			f.afterAdmit()
		}
		if f.racedAfterGate {
			// Another request created the job while the gate decided: it is returned and the admission refunded.
			request.Gate.Refund()
			return &reacquire.Job{ID: "raced", EntryID: request.EntryID, FileID: request.FileID}, nil
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
