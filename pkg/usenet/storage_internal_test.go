package usenet

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/fsutil"
)

// NZB ids come from API callers; one that names a path is refused instead of
// reading or deleting a file outside the meta directory.
func TestNZBStorageRefusesPathIDs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	victim := filepath.Join(root, "victim.meta")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewNZBStorage(filepath.Join(root, "meta"), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../victim", "", "a/b"} {
		if _, getErr := store.GetNZB(id); !errors.Is(getErr, fsutil.ErrUnsafeName) {
			t.Errorf("GetNZB(%q) = %v, want ErrUnsafeName", id, getErr)
		}
		if deleteErr := store.DeleteNZB(id); !errors.Is(deleteErr, fsutil.ErrUnsafeName) {
			t.Errorf("DeleteNZB(%q) = %v, want ErrUnsafeName", id, deleteErr)
		}
		if store.Exists(id) {
			t.Errorf("Exists(%q) = true", id)
		}
	}
	if _, statErr := os.Stat(victim); statErr != nil {
		t.Fatalf("a file outside the meta directory was removed: %v", statErr)
	}
}
