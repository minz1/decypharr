package main

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// suppressionDirective is the comment golangci-lint reads as a suppression,
// assembled so this file's own source does not contain it.
const suppressionDirective = "//" + "nolint"

// TestNoLintSuppressions keeps the module free of lint suppressions: a
// finding is fixed at its cause, never silenced. It parses every Go file's
// comments, so strings that merely mention the directive do not count.
func TestNoLintSuppressions(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		switch {
		case walkErr != nil:
			return walkErr
		case entry.IsDir():
			if skipDir(path, entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		case filepath.Ext(path) != ".go":
			return nil
		}
		return checkFile(t, fset, path)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// skipDir leaves out hidden directories (.git) and node_modules.
func skipDir(path, name string) bool {
	return path != "." && (strings.HasPrefix(name, ".") || name == "node_modules")
}

// checkFile reports every suppression comment in one Go file.
func checkFile(t *testing.T, fset *token.FileSet, path string) error {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return err
	}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if strings.Contains(comment.Text, suppressionDirective) {
				t.Errorf("%s: lint suppression %q; fix the finding instead", fset.Position(comment.Pos()), comment.Text)
			}
		}
	}
	return nil
}
