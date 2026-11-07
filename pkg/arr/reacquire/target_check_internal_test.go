package reacquire

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// targetRig is a managed mount and a library directory beside it.
type targetRig struct {
	t       *testing.T
	root    string
	library string
}

func newTargetRig(t *testing.T) *targetRig {
	t.Helper()
	base := t.TempDir()
	rig := &targetRig{t: t, root: filepath.Join(base, "mnt", "__all__"), library: filepath.Join(base, "library")}
	for _, dir := range []string{rig.root, rig.library} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return rig
}

// link makes a library symlink to a path in the mount; the target need not exist.
func (rig *targetRig) link(name, target string) string {
	rig.t.Helper()
	path := filepath.Join(rig.library, name)
	if err := os.Symlink(target, path); err != nil {
		rig.t.Fatal(err)
	}
	return path
}

func (rig *targetRig) want() TargetWant {
	return TargetWant{Bound: true, Folder: "Show.S01", Name: "ep1.mkv"}
}

// The live library path is checked before the Arr file is deleted: it must be
// a symlink into the managed mount that points at the bound entry's file.
func TestLibraryTargetCheckGuardsTheArrFileDelete(t *testing.T) {
	t.Parallel()
	rig := newTargetRig(t)
	regular := filepath.Join(rig.library, "regular.mkv")
	if err := os.WriteFile(regular, []byte("not a link"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := rig.link("outside.mkv", filepath.Join(rig.library, "elsewhere", "ep1.mkv"))
	otherEntry := rig.link("other.mkv", filepath.Join(rig.root, "Other.S01", "ep1.mkv"))
	otherName := rig.link("name.mkv", filepath.Join(rig.root, "Show.S01", "ep2.mkv"))
	dangling := rig.link("dangling.mkv", filepath.Join(rig.root, "Show.S01", "ep1.mkv"))
	live := rig.link("live.mkv", filepath.Join(rig.root, "Show.S01", "ep1.mkv"))
	if err := os.MkdirAll(filepath.Join(rig.root, "Show.S01"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig.root, "Show.S01", "ep1.mkv"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, test := range map[string]struct {
		path    string
		refused bool
	}{
		"a real file":                       {path: regular, refused: true},
		"a link outside the mount":          {path: outside, refused: true},
		"a link to another entry":           {path: otherEntry, refused: true},
		"a link to another file":            {path: otherName, refused: true},
		"a path that does not exist":        {path: filepath.Join(rig.library, "missing.mkv"), refused: true},
		"a path below a regular file":       {path: filepath.Join(regular, "child.mkv"), refused: true},
		"a link to the bound file":          {path: live},
		"a dangling link to the bound file": {path: dangling},
	} {
		err := CheckLibraryTarget(test.path, rig.root, rig.want())
		if test.refused != errors.Is(err, ErrOperatorRequired) || (!test.refused && err != nil) {
			t.Errorf("%s: CheckLibraryTarget() = %v, want refused = %v", name, err, test.refused)
		}
	}
}

// A file bound only by name and size sits in a folder that has moved: any
// folder is fine unless it belongs to another entry.
func TestLibraryTargetCheckAllowsAMovedFolderOnlyIfNoOtherEntryOwnsIt(t *testing.T) {
	t.Parallel()
	rig := newTargetRig(t)
	moved := rig.link("moved.mkv", filepath.Join(rig.root, "Old.Folder", "ep1.mkv"))
	want := rig.want()
	want.Moved = true

	if err := CheckLibraryTarget(moved, rig.root, want); err != nil {
		t.Fatalf("a moved folder = %v, want accepted", err)
	}
	want.OwnedElsewhere = func(folder, name string) bool { return folder == "Old.Folder" && name == "ep1.mkv" }
	if err := CheckLibraryTarget(moved, rig.root, want); !errors.Is(err, ErrOperatorRequired) {
		t.Fatalf("another entry's folder = %v, want refused", err)
	}
}

// A file the Arr holds without a managed entry only has to point into the mount.
func TestLibraryTargetCheckOfAnUnboundFile(t *testing.T) {
	t.Parallel()
	rig := newTargetRig(t)
	inside := rig.link("inside.mkv", filepath.Join(rig.root, "Anything", "x.mkv"))
	outside := rig.link("outside.mkv", filepath.Join(rig.library, "x.mkv"))

	if err := CheckLibraryTarget(inside, rig.root, TargetWant{}); err != nil {
		t.Fatalf("link into the mount = %v", err)
	}
	if err := CheckLibraryTarget(outside, rig.root, TargetWant{}); !errors.Is(err, ErrOperatorRequired) {
		t.Fatalf("link outside the mount = %v, want refused", err)
	}
}

// targetGuard is an invalidator that guards the library targets.
type targetGuard struct {
	refuse error
	seen   []Binding
}

func (guard *targetGuard) InvalidateReacquire(context.Context, Job) error { return nil }

func (guard *targetGuard) CheckLibraryTarget(binding Binding) error {
	guard.seen = append(guard.seen, binding)
	return guard.refuse
}

// A refused target sends nothing to the Arr and stops the job for an operator.
func TestRefusedLibraryTargetSendsNothing(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)
	guard := &targetGuard{refuse: OperatorRequired("the library path is a real file")}
	rig.handler = NewHandler(rig.arrs, guard)

	stopped := rig.run(t)
	if stopped.Status != StatusNeedsAttention {
		t.Fatalf("job = %s: %s", stopped.Status, stopped.LastError)
	}
	if deletes, searches := fake.deleteCount(), fake.searchCount(); deletes != 0 || searches != 0 {
		t.Fatalf("deletes = %d, searches = %d, want nothing sent", deletes, searches)
	}
	if len(guard.seen) != 1 || guard.seen[0].ArrFileID != fakeSonarrFileID {
		t.Fatalf("guard saw %+v, want the binding of Arr file %d", guard.seen, fakeSonarrFileID)
	}
}

func TestAcceptedLibraryTargetLetsTheDeleteThrough(t *testing.T) {
	t.Parallel()
	fake := newFakeSonarr(t, 101)
	rig := newFakeSonarrRig(t, fake, StrategyCommandSearch, 101)
	rig.handler = NewHandler(rig.arrs, &targetGuard{})

	if waiting := rig.run(t); waiting.Status != StatusWaitingForGrab || fake.deleteCount() != 1 {
		t.Fatalf("job = %s, deletes = %d: %s", waiting.Status, fake.deleteCount(), waiting.LastError)
	}
}
