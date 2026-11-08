package usenet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"
)

// cancelAfterCtx reports no error for the first free calls to Err, then
// [context.Canceled] for ever: a cancellation that lands at a known point.
type cancelAfterCtx struct {
	context.Context

	mu   sync.Mutex
	free int
}

func (c *cancelAfterCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.free > 0 {
		c.free--
		return nil
	}
	return context.Canceled
}

func TestMigrateLegacyCancelledWritesNoMarker(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := &NZBStorage{metaDir: dir, codec: testCodec(t), logger: zerolog.Nop()}
	for _, id := range []string{"a", "b", "c"} {
		data, err := proto.Marshal(&NZBProto{Id: id, Name: id})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, id+metaFileExtension), data, metaFileMode); err != nil {
			t.Fatal(err)
		}
	}

	ctx := &cancelAfterCtx{Context: t.Context(), free: 1}
	migrated, err := store.MigrateLegacy(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("MigrateLegacy cancelled after the first file = %d, %v; want context.Canceled", migrated, err)
	}
	if migrated != 1 {
		t.Fatalf("migrated %d files before the cancel, want 1", migrated)
	}
	if store.migrationMarkerExists() {
		t.Fatal("a cancelled migration wrote its completion marker")
	}

	migrated, err = store.MigrateLegacy(t.Context())
	if err != nil || migrated != 2 {
		t.Fatalf("rerun = %d, %v; want the 2 remaining files", migrated, err)
	}
	if !store.migrationMarkerExists() {
		t.Fatal("the completed rerun wrote no marker")
	}
	legacy, err := store.legacyMetaFiles()
	if err != nil || len(legacy) != 0 {
		t.Fatalf("legacy files left after the rerun: %v, %v", legacy, err)
	}
}
