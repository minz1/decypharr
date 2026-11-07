package reacquire

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// LibraryTargetChecker is an Invalidator that guards the Arr file deletes: the handler asks it about each binding's
// live library path before any Arr file is deleted.
type LibraryTargetChecker interface {
	CheckLibraryTarget(Binding) error
}

// TargetWant is what a library path must resolve to before the Arr file at it is deleted.
type TargetWant struct {
	// Bound is false for a file the Arr holds without a managed entry: any file of the mount will do.
	Bound bool
	// Folder and Name are the bound entry file's current entry folder and file name.
	Folder, Name string
	// Moved lets the folder differ: a file bound by name and size sits in a folder that has moved.
	Moved bool
	// OwnedElsewhere, when set, reports a folder and name that belong to another managed entry.
	OwnedElsewhere func(folder, name string) bool
}

// missing reports a path that does not exist, as against one that cannot be read.
func missing(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// CheckLibraryTarget reads the live library path, which the Arr's record of the file may no longer match, and
// refuses with OperatorRequired unless it is a symlink into the managed mount that resolves to the bound file by the
// rules of the index (entry folder and name). It reads the link and never its target, so a link whose data vanished
// still passes and the file heals; no size is compared.
func CheckLibraryTarget(path, managedRoot string, want TargetWant) error {
	managedRoot = filepath.Clean(managedRoot)
	target, readable := symlinkTarget(path)
	switch {
	case target == "" && !readable && missing(path):
		return OperatorRequired(fmt.Sprintf(
			"the library link %q is gone, so its Arr file was not deleted", path,
		))
	case target == "" && !readable:
		return OperatorRequired(fmt.Sprintf(
			"the library path %q cannot be read, so its Arr file was not deleted", path,
		))
	case target == "":
		return OperatorRequired(fmt.Sprintf(
			"the library path %q is not a symlink into the managed mount (a real file or directory), so its Arr file was not deleted",
			path,
		))
	case managedRoot != "." && !isUnder(target, managedRoot):
		return OperatorRequired(fmt.Sprintf(
			"the library path %q points at %q, outside the managed mount, so its Arr file was not deleted",
			path, target,
		))
	case !want.Bound:
		return nil
	}
	name := filepath.Base(target)
	folder := entryFolderOf(target, managedRoot)
	if name != want.Name {
		return OperatorRequired(fmt.Sprintf(
			"the library path %q points at %q, not at the bound file %q, so its Arr file was not deleted",
			path, name, want.Name,
		))
	}
	if folder == want.Folder {
		return nil
	}
	if want.Moved && (want.OwnedElsewhere == nil || !want.OwnedElsewhere(folder, name)) {
		return nil
	}
	return OperatorRequired(fmt.Sprintf(
		"the library path %q points into the folder %q, not the bound entry's folder %q, so its Arr file was not deleted",
		path,
		folder,
		want.Folder,
	))
}
