// Package fsutil creates the directory trees decypharr shares with other
// services (the *arr apps, media servers) through a common group.
package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// MkdirShared creates path and any missing parents with mode's permission
// bits (less the process umask). When mode has the setgid bit, every
// directory it creates gets it too, so files and directories created inside
// inherit the directory's group.
func MkdirShared(path string, mode fs.FileMode) error {
	created := missingDirs(path)
	if err := os.MkdirAll(path, mode.Perm()); err != nil {
		return err
	}
	if mode&fs.ModeSetgid == 0 {
		return nil
	}
	for _, dir := range created {
		if err := addSetgid(dir); err != nil {
			return err
		}
	}
	return nil
}

// missingDirs lists path and its ancestors that do not exist yet, outermost
// first.
func missingDirs(path string) []string {
	var missing []string
	for dir := filepath.Clean(path); ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
			break
		}
		missing = append([]string{dir}, missing...)
		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}
	return missing
}

// addSetgid sets the setgid bit on dir, keeping its permission bits (mkdir
// on Linux ignores setgid in its mode argument).
func addSetgid(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSetgid != 0 {
		return nil
	}
	return os.Chmod(dir, info.Mode().Perm()|fs.ModeSetgid)
}
