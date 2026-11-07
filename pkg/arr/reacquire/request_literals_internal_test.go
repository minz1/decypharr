package reacquire

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRequestLiteralsNameTheirStrategy keeps every reacquisition request in
// production code explicit about what it does to the Arr: a request that
// leaves the strategy out takes the default, and a caller adding a literal
// should decide between a plain search and a blocklist.
func TestRequestLiteralsNameTheirStrategy(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		switch {
		case walkErr != nil:
			return walkErr
		case entry.IsDir():
			name := entry.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		case filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}
		for _, literal := range requestLiteralsWithoutStrategy(file) {
			t.Errorf("%s: reacquire request literal does not set Strategy", fset.Position(literal.Pos()))
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no Go files were checked")
	}
}

func TestRequestLiteralScanFindsAMissingStrategy(t *testing.T) {
	t.Parallel()
	const source = `package other

import "example.com/decypharr/pkg/arr/reacquire"

var (
	explicit = reacquire.Request{EntryID: "e", Strategy: reacquire.StrategyCommandSearch}
	library  = reacquire.LibraryRequest{ArrName: "a", Strategy: reacquire.StrategyCommandSearch}
	omitted  = reacquire.Request{EntryID: "e"}
	empty    = reacquire.Request{}
	missing  = reacquire.LibraryRequest{ArrName: "a"}
	unrelated = struct{ Strategy string }{}
)
`
	file, err := parser.ParseFile(token.NewFileSet(), "other.go", source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(requestLiteralsWithoutStrategy(file)); got != 3 {
		t.Fatalf("literals without a strategy = %d, want 3", got)
	}
}

// requestLiteralsWithoutStrategy returns the Request and LibraryRequest
// composite literals of file that have no Strategy key. Inside package
// reacquire the types are named without a qualifier.
func requestLiteralsWithoutStrategy(file *ast.File) []*ast.CompositeLit {
	inPackage := file.Name.Name == "reacquire"
	var found []*ast.CompositeLit
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if !ok || !isRequestType(literal.Type, inPackage) || setsStrategy(literal) {
			return true
		}
		found = append(found, literal)
		return true
	})
	return found
}

func isRequestType(expr ast.Expr, inPackage bool) bool {
	switch typed := expr.(type) {
	case *ast.SelectorExpr:
		pkg, ok := typed.X.(*ast.Ident)
		return ok && pkg.Name == "reacquire" && (typed.Sel.Name == "Request" || typed.Sel.Name == "LibraryRequest")
	case *ast.Ident:
		return inPackage && (typed.Name == "Request" || typed.Name == "LibraryRequest")
	default:
		return false
	}
}

func setsStrategy(literal *ast.CompositeLit) bool {
	for _, element := range literal.Elts {
		pair, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, isIdent := pair.Key.(*ast.Ident); isIdent && key.Name == "Strategy" {
			return true
		}
	}
	return false
}
