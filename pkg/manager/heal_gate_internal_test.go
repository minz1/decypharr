package manager

import (
	"errors"
	"testing"
	"testing/synctest"

	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/repair"
)

// capacityLeft counts how many more heals the dropped bucket admits before its cap.
func capacityLeft(t *testing.T, service *repair.Service) int {
	t.Helper()
	left := 0
	for left < 100 {
		err := service.Admit(t.Context(), repair.HealRequest{Provider: "realdebrid", Class: repair.ClassDropped})
		if errors.Is(err, repair.ErrHealCapped) {
			return left
		}
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		left++
	}
	return left
}

// A file whose reacquisition fails before a job is created asks nothing of the breaker, so an entry that keeps
// failing does not use up the caps on every sync.
func TestDroppedFilesThatFailBeforeAJobCostNothing(t *testing.T) {
	t.Parallel()
	entry, recovery := droppedEntryWithFiles(10)
	recovery.failBeforeGate = errors.New("binding unusable")
	m, _ := newDroppedTestManager(t, recovery)
	m.repair = repair.New(testGroup(t), repair.Dependencies{Storage: m.storage})

	for range 6 {
		if err := m.recoverDroppedEntry("realdebrid", entry); err == nil {
			t.Fatal("a failing reacquisition reported success")
		}
	}
	if left := capacityLeft(t, m.repair); left != 25 {
		t.Fatalf("capacity left = %d, want all 25: failures before any delete must not use the caps", left)
	}
}

// If another request wins the job while the gate decided, the admission is given back.
func TestDroppedFilesThatLoseTheRaceAreRefunded(t *testing.T) {
	t.Parallel()
	entry, recovery := droppedEntryWithFiles(10)
	recovery.racedAfterGate = true
	m, _ := newDroppedTestManager(t, recovery)
	m.repair = repair.New(testGroup(t), repair.Dependencies{Storage: m.storage})

	if err := m.recoverDroppedEntry("realdebrid", entry); err != nil {
		t.Fatal(err)
	}
	if left := capacityLeft(t, m.repair); left != 25 {
		t.Fatalf("capacity left = %d, want all 25: no job was created, so nothing was deleted", left)
	}
}

// A stream failure on a file that already has a job gets that job back even from a tripped bucket; one without a
// job is held.
func TestStreamReacquireIsGatedOnlyForANewJob(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		recovery := &filesRecovery{entryID: "nzb-1", files: map[string]bool{"file-1": true, "file-2": true}}
		recovery.active = []reacquire.Job{
			{ID: "job-0", EntryID: "nzb-1", FileID: "file-1", Status: reacquire.StatusQueued},
		}
		m := &Manager{work: testGroup(t), checkStreamFile: confirmBroken}
		m.SetArrRecovery(recovery)
		m.repair = repair.New(testGroup(t), repair.Dependencies{})
		tripBucket(t, m.repair, m.repair.ProviderFor("nzb-1"), repair.ClassUsenetSegment)

		m.submitStreamReacquire("nzb-1", "file-1", "a.mkv")
		synctest.Wait()
		m.submitStreamReacquire("nzb-1", "file-2", "b.mkv")
		synctest.Wait()

		if len(recovery.vetoes) != 1 || !errors.Is(recovery.vetoes[0], repair.ErrHealHeld) {
			t.Fatalf("vetoes = %v, want the one new file held and the one with a job left alone", recovery.vetoes)
		}
		if recovery.calls.Load() != 0 {
			t.Fatalf("jobs created = %d, want none from a tripped bucket", recovery.calls.Load())
		}
	})
}
