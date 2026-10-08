package migration

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// Stop cancels a running migration instead of waiting for it: Start used to
// hold the lock Stop needs for the whole run.
func TestStopCancelsRunningMigration(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	debridDir := filepath.Join(dataDir, "cache", "realdebrid")
	if err := os.MkdirAll(debridDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		body := fmt.Sprintf(`{"id":"%d","info_hash":"%040d","name":"t%d"}`, i, i, i)
		if err := os.WriteFile(filepath.Join(debridDir, fmt.Sprintf("%d.json", i)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	m := New(store, dataDir, zerolog.Nop())
	stopped := make(chan error, 1)
	calls := 0
	m.beforeEach = func() {
		calls++
		if calls != 1 {
			return
		}
		go func() { stopped <- m.Stop() }()
		select {
		case stopErr := <-stopped:
			stopped <- stopErr
		case <-time.After(5 * time.Second):
			t.Error("Stop waited for the running migration")
		}
	}

	if startErr := m.Start(); startErr != nil {
		t.Fatal(startErr)
	}
	if stopErr := <-stopped; stopErr != nil {
		t.Fatalf("Stop: %v", stopErr)
	}
	if calls != 1 {
		t.Fatalf("migration went on for %d torrents after Stop, want it to stop after the first", calls-1)
	}
}
