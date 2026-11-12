package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoBareGoStatements keeps every goroutine owned: production code starts one through internal/task (a task
// group or a keyed runner) or a scoped wait group, never with a bare `go`, so shutdown can wait for it.
func TestNoBareGoStatements(t *testing.T) {
	t.Parallel()
	for _, v := range bareGoStatements(t, ".") {
		t.Error(v)
	}
}

// TestNoContextBackground keeps every context a child of the root: only main, internal/task and tests create one
// from [context.Background], so a stop reaches all work.
func TestNoContextBackground(t *testing.T) {
	t.Parallel()
	for _, v := range backgroundContexts(t, ".") {
		t.Error(v)
	}
}

// TestNoAskInsideKeyedHandlers keeps keyed handlers and their child tasks from calling Ask: keyed.Ask refuses a
// call from a handler only when it carries the handler's own ctx, so an Ask with any other ctx blocks the key on
// itself until the timeout.
func TestNoAskInsideKeyedHandlers(t *testing.T) {
	t.Parallel()
	for _, v := range asksInsideHandlers(t, ".") {
		t.Error(v)
	}
}

// TestConcurrencyGuardsCatchInjectedViolations proves each guard fires on a violation and leaves the exempt places
// (tests, package main, internal/task, test helpers, an Ask outside any handler) alone.
func TestConcurrencyGuardsCatchInjectedViolations(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSource(t, root, "pkg/a/a.go", `package a

import "context"

func start() {
	go work()
	_ = context.Background()
}

type owner struct{ r runner }

func (o owner) Handle(ctx context.Context, key string, st *int, m int, fx *keyed.Effects[string, int]) {
	fx.Go("child", func(ctx context.Context) int { return o.lookup(ctx) }, nil)
}

func (o owner) lookup(ctx context.Context) int {
	v, _ := o.r.Ask(ctx, "k", 1)
	return v.(int)
}
`)
	writeSource(t, root, "pkg/b/b.go", `package b

func caller(ctx context.Context, r runner) { _, _ = r.Ask(ctx, "k", 1) }
`)
	writeSource(t, root, "pkg/c/c.go", `package c

import stdctx "context"

var root = stdctx.TODO()
`)
	const exempt = "\n\nfunc f() { go work(); _ = context.Background() }\n"
	writeSource(t, root, "pkg/a/a_test.go", "package a"+exempt)
	writeSource(t, root, "cmd/tool/main.go", "package main"+exempt)
	writeSource(t, root, "internal/task/task.go", "package task"+exempt)
	writeSource(t, root, "internal/testutil/srv/srv.go", "package srv"+exempt)
	writeSource(t, root, "pkg/a/atest/fake.go", "package atest"+exempt)

	if got := bareGoStatements(t, root); len(got) != 1 || !strings.Contains(got[0], "a.go:6") {
		t.Errorf("bare go guard = %q, want one finding at a.go:6", got)
	}
	got := strings.Join(backgroundContexts(t, root), "\n")
	if strings.Count(got, "\n") != 1 || !strings.Contains(got, "a.go:7") || !strings.Contains(got, "c.go:5") {
		t.Errorf("context.Background guard = %q, want findings at a.go:7 and c.go:5 (an aliased TODO)", got)
	}
	if got := asksInsideHandlers(t, root); len(got) != 1 || !strings.Contains(got[0], "a.go:17") {
		t.Errorf("Ask guard = %q, want one finding at a.go:17 (reached from Handle through its child task)", got)
	}
}

func writeSource(t *testing.T, root, rel, src string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
}

func bareGoStatements(t *testing.T, root string) []string {
	t.Helper()
	return inspectGoverned(t, root, func(_ *ast.File, node ast.Node) string {
		if _, ok := node.(*ast.GoStmt); ok {
			return "bare go statement; start it through internal/task or a scoped sync.WaitGroup.Go"
		}
		return ""
	})
}

func backgroundContexts(t *testing.T, root string) []string {
	t.Helper()
	return inspectGoverned(t, root, func(file *ast.File, node ast.Node) string {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Background" && sel.Sel.Name != "TODO") {
			return ""
		}
		if pkg, isIdent := sel.X.(*ast.Ident); isIdent && pkg.Name == contextImportName(file) {
			return "context." + sel.Sel.Name + "; derive the context from the caller or the owning task group"
		}
		return ""
	})
}

// contextImportName is the name file refers to package context by ("" when it does not import it by name).
func contextImportName(file *ast.File) string {
	for _, spec := range file.Imports {
		if spec.Path.Value != `"context"` {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name
		}
		return "context"
	}
	return ""
}

// inspectGoverned reports, for every node of a governed file, the finding check returns ("" for none).
func inspectGoverned(t *testing.T, root string, check func(*ast.File, ast.Node) string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var found []string
	for _, files := range governedPackages(t, root, fset) {
		for _, file := range files {
			ast.Inspect(file, func(node ast.Node) bool {
				if msg := check(file, node); msg != "" {
					found = append(found, fset.Position(node.Pos()).String()+": "+msg)
				}
				return true
			})
		}
	}
	return found
}

// asksInsideHandlers reports every Ask call reachable from a function that takes a *keyed.Effects (a Handle method
// and its helpers, including the child functions they pass to Effects.Go), following calls and function values by
// name within the package.
// shortcut: name-based and package-local (no type information), so a call into another package is not followed;
// upgrade to go/types if a handler starts calling into another package that uses Ask.
func asksInsideHandlers(t *testing.T, root string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var found []string
	for _, files := range governedPackages(t, root, fset) {
		decls, queue := handlerRoots(files)
		seen := make(map[*ast.FuncDecl]bool)
		for len(queue) > 0 {
			fn := queue[0]
			queue = queue[1:]
			if !seen[fn] {
				seen[fn] = true
				queue = append(queue, scanForAsk(fset, fn, decls, &found)...)
			}
		}
	}
	return found
}

// handlerRoots indexes a package's functions by name and returns those that take a *keyed.Effects.
func handlerRoots(files []*ast.File) (map[string][]*ast.FuncDecl, []*ast.FuncDecl) {
	decls := make(map[string][]*ast.FuncDecl)
	var roots []*ast.FuncDecl
	for _, file := range files {
		for _, d := range file.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil {
				decls[fn.Name.Name] = append(decls[fn.Name.Name], fn)
				if takesEffects(fn) {
					roots = append(roots, fn)
				}
			}
		}
	}
	return decls, roots
}

// scanForAsk records every Ask selector in fn's body and returns the package functions fn names (calls or values).
func scanForAsk(
	fset *token.FileSet,
	fn *ast.FuncDecl,
	decls map[string][]*ast.FuncDecl,
	found *[]string,
) []*ast.FuncDecl {
	var next []*ast.FuncDecl
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.SelectorExpr:
			if n.Sel.Name == "Ask" {
				*found = append(*found, fset.Position(n.Sel.Pos()).String()+": Ask in "+fn.Name.Name+
					", reachable from a keyed handler; send a message and handle the reply as one instead")
			}
			next = append(next, decls[n.Sel.Name]...)
		case *ast.Ident:
			next = append(next, decls[n.Name]...)
		}
		return true
	})
	return next
}

func takesEffects(fn *ast.FuncDecl) bool {
	for _, field := range fn.Type.Params.List {
		if strings.Contains(types.ExprString(field.Type), "keyed.Effects[") {
			return true
		}
	}
	return false
}

// governedPackages parses the Go files the concurrency rules govern, grouped by directory: everything except
// tests, package main, internal/task (which owns the goroutines and the root context) and test helpers
// (internal/testutil and packages named *test).
func governedPackages(t *testing.T, root string, fset *token.FileSet) map[string][]*ast.File {
	t.Helper()
	packages := make(map[string][]*ast.File)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if skipDir(rel, entry.Name()) || rel == "internal/task" || rel == "internal/testutil" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		if pkg := file.Name.Name; pkg == "main" || strings.HasSuffix(pkg, "test") {
			return nil
		}
		packages[filepath.Dir(rel)] = append(packages[filepath.Dir(rel)], file)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return packages
}
