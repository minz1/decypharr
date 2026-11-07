package storage_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestHeldEntryHealthIsAlwaysDueAndIsNotBroken(t *testing.T) {
	t.Parallel()
	now := time.Now()
	held := &storage.EntryHealth{
		EntryName: "held", Status: storage.HealthHeld, LastCheckedAt: now,
		NextCheckDueAt: now.Add(7 * 24 * time.Hour),
		BrokenFiles:    []storage.BrokenFile{{FileName: "a.mkv", InfoHash: "h"}},
	}
	if !held.IsDue(now, 7*24*time.Hour) {
		t.Fatal("a held entry must be re-probed by the next run")
	}
	if held.Status == storage.HealthBroken {
		t.Fatal("a held entry must never look broken to a binary that only knows the older statuses")
	}
}

func TestEntryHealthKeepsTheObservationFieldsAndReadsOldRows(t *testing.T) {
	t.Parallel()
	store, err := storage.NewStorage(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	first := time.Date(2026, 11, 7, 1, 2, 3, 0, time.UTC)
	want := &storage.EntryHealth{
		EntryName: "e", Status: storage.HealthHeld, HeldBucket: "realdebrid/hoster_link", HeldAt: first,
		BrokenFiles: []storage.BrokenFile{{
			FileName: "a.mkv", Provider: "realdebrid", FirstSeenAt: first, FirstSeenRunID: "run-1",
		}},
	}
	if saveErr := store.SaveEntryHealth(want); saveErr != nil {
		t.Fatal(saveErr)
	}
	got, err := store.GetEntryHealth("e")
	if err != nil {
		t.Fatal(err)
	}
	file := got.BrokenFiles[0]
	if got.Status != storage.HealthHeld || got.HeldBucket != want.HeldBucket || !got.HeldAt.Equal(first) ||
		!file.FirstSeenAt.Equal(first) || file.FirstSeenRunID != "run-1" || file.Provider != "realdebrid" {
		t.Fatalf("round trip = %#v", got)
	}

	var old storage.EntryHealth
	raw := `{"entry_name":"o","status":"broken","broken_files":[{"entry_name":"o","file_name":"a.mkv"}]}`
	if unmarshalErr := json.Unmarshal([]byte(raw), &old); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if !old.BrokenFiles[0].FirstSeenAt.IsZero() || old.BrokenFiles[0].FirstSeenRunID != "" {
		t.Fatalf("an old row decoded to %#v, want no first observation", old.BrokenFiles[0])
	}
}

func TestCountEntryHealthByStatusSeesMutations(t *testing.T) {
	t.Parallel()
	store, err := storage.NewStorage(t.TempDir(), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if saveErr := store.SaveEntryHealth(
		&storage.EntryHealth{EntryName: "a", Status: storage.HealthBroken},
	); saveErr != nil {
		t.Fatal(saveErr)
	}
	if got := store.CountEntryHealthByStatus()[storage.HealthBroken]; got != 1 {
		t.Fatalf("broken = %d, want 1", got)
	}

	// The histogram is cached; a save or clear must not be hidden behind it.
	if saveErr := store.SaveEntryHealth(
		&storage.EntryHealth{EntryName: "b", Status: storage.HealthBroken},
	); saveErr != nil {
		t.Fatal(saveErr)
	}
	if got := store.CountEntryHealthByStatus()[storage.HealthBroken]; got != 2 {
		t.Fatalf("broken after save = %d, want 2", got)
	}
	if _, clearErr := store.ClearEntryHealthByStatuses([]storage.HealthStatus{storage.HealthBroken}); clearErr != nil {
		t.Fatal(clearErr)
	}
	if got := store.CountEntryHealthByStatus()[storage.HealthBroken]; got != 0 {
		t.Fatalf("broken after clear = %d, want 0", got)
	}
}
