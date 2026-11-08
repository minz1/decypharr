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

// A cancelled context stops a running migration: Start returns and no
// further torrent migrates.
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
		if calls == 1 {
			cancel()
		}
	}

	if startErr := m.Start(ctx); startErr != nil {
		t.Fatal(startErr)
	}
	if calls != 1 {
		t.Fatalf("migration went on for %d torrents after the cancel, want it to stop after the first", calls-1)
	}
	status, err := m.GetStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Running {
		t.Fatal("status still Running after the cancelled migration returned")
	}
}
