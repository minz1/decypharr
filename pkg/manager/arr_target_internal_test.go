package manager

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// targetHarness has two stored entries, a library directory and the managed mount.
type targetHarness struct {
	*pruneHarness

	library string
	folders map[string]string
}

func newTargetHarness(t *testing.T) *targetHarness {
	t.Helper()
	h := &targetHarness{pruneHarness: newPruneHarness(t, config.PruneModeDelete), library: t.TempDir()}
	h.mount2(t)
	h.folders = map[string]string{}
	for _, hash := range []string{"one", "two"} {
		entry := h.addEntry(t, hash, func(e *storage.Entry) {
			e.Files = map[string]*storage.File{
				"ep.mkv": {ID: "f-" + hash, Name: "ep.mkv", Size: 10, InfoHash: hash},
			}
		})
		h.folders[hash] = entry.GetFolder(h.m.folderNaming())
	}
	return h
}

// link makes a library symlink to the file name in an entry's folder of the mount.
func (h *targetHarness) link(t *testing.T, libraryName, entry string) string {
	t.Helper()
	path := filepath.Join(h.library, libraryName)
	target := filepath.Join(h.mount, EntryAllFolder, h.folders[entry], "ep.mkv")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func (h *targetHarness) binding(path string, confidence reacquire.Confidence) reacquire.Binding {
	return reacquire.Binding{
		EntryID: "one", EntryFileID: "f-one", EntryFileName: "ep.mkv", LibraryPath: path, Confidence: confidence,
	}
}

func TestLibraryTargetIsCheckedAgainstTheBoundEntry(t *testing.T) {
	t.Parallel()
	h := newTargetHarness(t)
	regular := filepath.Join(h.library, "regular.mkv")
	if err := os.WriteFile(regular, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		path       string
		confidence reacquire.Confidence
		refused    bool
	}{
		"a real file":                   {path: regular, confidence: reacquire.ConfidenceExactPath, refused: true},
		"a link to another entry":       {path: h.link(t, "other.mkv", "two"), confidence: reacquire.ConfidenceExactPath, refused: true},
		"a link to another entry moved": {path: h.link(t, "moved-other.mkv", "two"), confidence: reacquire.ConfidenceManagedTarget, refused: true},
		"a path that cannot be read":    {path: filepath.Join(regular, "child.mkv"), confidence: reacquire.ConfidenceExactPath, refused: true},
		// The link is read, not followed: no data sits behind it, and it still heals.
		"a dangling link to the entry": {path: h.link(t, "bound.mkv", "one"), confidence: reacquire.ConfidenceExactPath},
	} {
		err := h.m.CheckLibraryTarget(h.binding(test.path, test.confidence))
		if test.refused != errors.Is(err, reacquire.ErrOperatorRequired) || (!test.refused && err != nil) {
			t.Errorf("%s: CheckLibraryTarget() = %v, want refused = %v", name, err, test.refused)
		}
	}
}

// A moved entry folder is fine for a file bound by name and size, as long as
// the folder is not another entry's.
func TestLibraryTargetOfAMovedFolder(t *testing.T) {
	t.Parallel()
	h := newTargetHarness(t)
	path := filepath.Join(h.library, "moved.mkv")
	if err := os.Symlink(filepath.Join(h.mount, EntryAllFolder, "Old.Folder", "ep.mkv"), path); err != nil {
		t.Fatal(err)
	}

	if err := h.m.CheckLibraryTarget(h.binding(path, reacquire.ConfidenceManagedTarget)); err != nil {
		t.Fatalf("moved folder = %v, want accepted", err)
	}
	err := h.m.CheckLibraryTarget(h.binding(path, reacquire.ConfidenceExactPath))
	if !errors.Is(err, reacquire.ErrOperatorRequired) {
		t.Fatalf("exact path in another folder = %v, want refused", err)
	}
}
