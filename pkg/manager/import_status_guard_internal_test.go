package manager

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

func importStatusUses(fset *token.FileSet, file *ast.File) []string {
	var found []string
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.Ident:
			if n.Name == "importStatusStarted" || n.Name == "importStatusError" {
				found = append(found, fset.Position(n.Pos()).String()+": "+n.Name+" is gone")
			}
		case *ast.TypeSpec:
			found = append(found, importRequestStatus(fset, n)...)
		}
		return true
	})
	return found
}

func importRequestStatus(fset *token.FileSet, spec *ast.TypeSpec) []string {
	fields, isStruct := spec.Type.(*ast.StructType)
	if spec.Name.Name != "ImportRequest" || !isStruct {
		return nil
	}
	var found []string
	for _, f := range fields.Fields.List {
		for _, name := range f.Names {
			if name.Name == "Status" {
				found = append(found, fset.Position(name.Pos()).String()+": ImportRequest.Status is gone")
			}
		}
	}
	return found
}

func TestImportStatusGone(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	for _, dir := range []string{".", filepath.Join("..", "server")} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			f, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			for _, v := range importStatusUses(fset, f) {
				t.Error(v)
			}
		}
	}
}
