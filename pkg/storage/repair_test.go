package storage_test

import (
	"testing"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

func TestCountEntryHealthByStatusSeesMutations(t *testing.T) {
	t.Parallel()
	store, err := storage.NewStorage(t.TempDir())
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
