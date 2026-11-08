package manager

import (
	"errors"
	"fmt"
	"testing"
	"testing/synctest"

	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/repair"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// tripBucket makes the repair breaker hold the bucket by confirming ten distinct entries in it.
func tripBucket(t *testing.T, service *repair.Service, provider string, class repair.Class) {
	t.Helper()
	var err error
	for i := range 10 {
		err = service.Admit(t.Context(), repair.HealRequest{
			Provider: provider, Class: class, Subject: fmt.Sprintf("tripping-%d", i),
		})
	}
	if !errors.Is(err, repair.ErrHealHeld) {
		t.Fatalf("setup: the bucket did not trip: %v", err)
	}
}

// A dropped torrent's Arr files are reacquired only through the breaker; a held heal is an error, so the entry is
// kept and the next sync tries again.
func TestDroppedFilesAreHeldWhileTheDroppedBucketIsTripped(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{
		binding: reacquire.Binding{EntryID: droppedHash, EntryFileID: "file-1"},
		started: make(chan struct{}),
	}
	m, entry := newDroppedTestManager(t, fake)
	m.repair = repair.New(testGroup(t), repair.Dependencies{Storage: m.storage})
	tripBucket(t, m.repair, "realdebrid", repair.ClassDropped)

	err := m.recoverDroppedEntry("realdebrid", entry)
	if !errors.Is(err, repair.ErrHealHeld) {
		t.Fatalf("recoverDroppedEntry = %v, want a held error so the entry is retried", err)
	}
	if fake.calls.Load() != 0 {
		t.Fatalf("reacquire calls = %d, want none while held", fake.calls.Load())
	}
	record, getErr := m.Queue().GetTorrent(droppedHash)
	if getErr != nil || record.State != storage.EntryStatePausedUP {
		t.Fatalf("queue record changed: %v %v", record, getErr)
	}
}

func TestDroppedFilesReacquireWhenTheBreakerIsClosed(t *testing.T) {
	t.Parallel()
	fake := &fakeArrRecovery{
		binding: reacquire.Binding{EntryID: droppedHash, EntryFileID: "file-1"},
		started: make(chan struct{}),
	}
	m, entry := newDroppedTestManager(t, fake)
	m.repair = repair.New(testGroup(t), repair.Dependencies{Storage: m.storage})

	if err := m.recoverDroppedEntry("realdebrid", entry); err != nil {
		t.Fatal(err)
	}
	if fake.calls.Load() != 1 {
		t.Fatalf("reacquire calls = %d, want 1", fake.calls.Load())
	}
}

func TestStreamReacquireIsHeldWhileTheUsenetBucketIsTripped(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		recovery := &fakeArrRecovery{
			started: make(chan struct{}),
			binding: reacquire.Binding{EntryID: "nzb-1", EntryFileID: "file-1"},
		}
		m := &Manager{work: testGroup(t), checkStreamFile: confirmBroken}
		m.SetArrRecovery(recovery)
		m.repair = repair.New(testGroup(t), repair.Dependencies{})
		tripBucket(t, m.repair, m.repair.ProviderFor("nzb-1"), repair.ClassUsenetSegment)

		m.submitStreamReacquire("nzb-1", "file-1", "a.mkv")
		synctest.Wait()
		if got := recovery.calls.Load(); got != 0 {
			t.Fatalf("reacquire calls while held = %d, want 0", got)
		}
	})
}
