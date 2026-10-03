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
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if name := entry.Name(); path != "." && (strings.HasPrefix(name, ".") || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		file, parseErr := parser.ParseFile(fset, path, src, parser.ParseComments)
		if parseErr != nil {
			return parseErr
		}
		for _, group := range file.Comments {
			for _, comment := range group.List {
				if strings.Contains(comment.Text, suppressionDirective) {
					t.Errorf("%s: lint suppression %q; fix the finding instead", fset.Position(comment.Pos()), comment.Text)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
