package migration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// A cancelled context stops a running migration: Start returns, no further
// torrent migrates, and the saved status stays Running so the next start
// resumes it (manager.migrate treats !Running && Completed > 0 as done).
func TestMigratorStartStopsWithCtx(t *testing.T) {
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

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := New(store, dataDir, zerolog.Nop())
	calls := 0
	m.beforeEach = func() {
		calls++
		if calls == 2 {
			cancel()
		}
	}

	if startErr := m.Start(ctx); startErr != nil {
		t.Fatal(startErr)
	}
	if calls != 2 {
		t.Fatalf("migration went on for %d torrents after the cancel, want it to stop at the second", calls-2)
	}
	status, err := m.GetStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Completed == 0 {
		t.Fatal("no torrent migrated before the cancel; the test needs a partial migration")
	}
	if !status.Running {
		t.Fatalf("cancelled partial migration (Completed %d) saved as not Running: the next start skips it",
			status.Completed)
	}
}
