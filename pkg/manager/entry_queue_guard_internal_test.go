package manager

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func queueActorFile(name string) bool { return name == "entry_actor.go" || name == "queue.go" }

func queueWriterName(call *ast.CallExpr) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	switch sel.Sel.Name {
	case "AddQueue", "UpdateQueue", "UpdateQueuedGrab", "UpdateWhereQueued", "DeleteWhereQueued", "RemoveQueued":
		return sel.Sel.Name
	case "Add", "Update", "Delete":
		if inner, isCall := sel.X.(*ast.CallExpr); isCall {
			if fun, isSel := inner.Fun.(*ast.SelectorExpr); isSel && fun.Sel.Name == "Queue" {
				return "Queue()." + sel.Sel.Name
			}
		}
		if lastName(sel.X) == "queue" {
			return "queue." + sel.Sel.Name
		}
	}
	return ""
}

func lastName(x ast.Expr) string {
	switch v := x.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	}
	return ""
}

func queueWrites(fset *token.FileSet, file *ast.File) []string {
	var found []string
	ast.Inspect(file, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if name := queueWriterName(call); name != "" {
				found = append(found, fset.Position(call.Pos()).String()+": "+name+
					"; a queue record is written only by the entry actor")
			}
		}
		return true
	})
	return found
}

func TestNoQueueWritesOutsideTheActor(t *testing.T) {
	t.Parallel()
	storageDir := filepath.Join("..", "storage")
	fset := token.NewFileSet()
	walkErr := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path == storageDir {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if filepath.Dir(path) == filepath.Join("..", "manager") && queueActorFile(d.Name()) {
			return nil
		}
		f, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}
		for _, v := range queueWrites(fset, f) {
			t.Error(v)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}

	src := `package manager

func (m *Manager) badStorage() { _ = m.storage.UpdateQueue(nil) }

func (m *Manager) badQueue() { _ = m.Queue().Add(nil) }

func (m *Manager) badField() { _ = m.queue.Delete("", false, nil) }

func (m *Manager) fine() { _ = m.storage.Get(""); _ = m.queue.GetTorrent(""); _ = m.DeleteQueued(nil, "", nil) }
`
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	if got := queueWrites(token.NewFileSet(), f); len(got) != 3 {
		t.Fatalf("guard findings %q, want the three bad ones", got)
	}
}
