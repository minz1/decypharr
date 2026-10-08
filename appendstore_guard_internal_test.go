package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppendstoreOpenedThroughKVStore keeps every appendstore behind
// internal/kvstore, whose ForEach works around appendstore's concurrent-scan
// race. A store opened directly would scan unguarded.
func TestAppendstoreOpenedThroughKVStore(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		switch {
		case walkErr != nil:
			return walkErr
		case entry.IsDir():
			if skipDir(path, entry.Name()) || path == filepath.Join("internal", "kvstore") {
				return filepath.SkipDir
			}
			return nil
		case filepath.Ext(path) != ".go":
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			sel, ok := node.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Open" {
				return true
			}
			if pkg, isIdent := sel.X.(*ast.Ident); isIdent && pkg.Name == "appendstore" {
				t.Errorf("%s: appendstore.Open; open stores with kvstore.Open", fset.Position(sel.Pos()))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// skipDir leaves out hidden directories (.git), node_modules and the root
// vendor directory (nix's buildGoModule fills it before the tests run).
func skipDir(path, name string) bool {
	return path != "." && (strings.HasPrefix(name, ".") || name == "node_modules" || path == "vendor")
}
