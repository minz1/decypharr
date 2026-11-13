package manager

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func goneClaimNames() []string {
	return []string{
		"processingEntries", "queueRestored", "claimDrivable", "torrentSubmissions", "JobQueue", "restoreEntry",
		"HandleExistingEntryMerge",
	}
}

func claimNames(fset *token.FileSet, file *ast.File) []string {
	var found []string
	ast.Inspect(file, func(node ast.Node) bool {
		if id, ok := node.(*ast.Ident); ok && slices.Contains(goneClaimNames(), id.Name) {
			found = append(found, fset.Position(id.Pos()).String()+": "+id.Name+
				"; the entry actor owns a grab, no claim map or job queue does")
		}
		return true
	})
	return found
}

func TestClaimMapsAreGone(t *testing.T) {
	t.Parallel()
	names, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, d := range names {
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			continue
		}
		f, parseErr := parser.ParseFile(fset, filepath.Join(".", d.Name()), nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		for _, v := range claimNames(fset, f) {
			t.Error(v)
		}
	}

	src := `package manager

type Manager struct{ processingEntries map[string]bool }

func (m *Manager) restoreEntry() { m.queue.JobQueue() }

func (m *Manager) fine() { m.entries.Ask() }
`
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	if got := claimNames(token.NewFileSet(), f); len(got) != 3 {
		t.Fatalf("guard findings %q, want the three bad ones", got)
	}
}
