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
	writeSource(t, root, "pkg/d/d.go", `package d

type deps struct{ fail func(int) }

type actor struct{ deps }

func wire(o owner) *actor { return &actor{deps: deps{fail: o.failAsk}} }

func (a *actor) Handle(ctx context.Context, key string, st *int, m int, fx *keyed.Effects[string, int]) {
	a.fail(m)
}

func (o owner) failAsk(m int) { _, _ = o.r.Ask(o.ctx, "k", m) }
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
	got = strings.Join(asksInsideHandlers(t, root), "\n")
	if strings.Count(got, "\n") != 1 || !strings.Contains(got, "a.go:17") || !strings.Contains(got, "d.go:13") {
		t.Errorf("Ask guard = %q, want findings at a.go:17 (through a child task) and d.go:13 (through a "+
			"dependency field)", got)
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
// and its helpers, including the child functions they pass to Effects.Go and the functions wired into dependency
// fields), following calls and function values within the package.
// shortcut: no type information, so a call into another package or on a local or parameter is not followed;
// upgrade to go/types if a handler starts reaching Ask that way.
func asksInsideHandlers(t *testing.T, root string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var found []string
	for _, files := range governedPackages(t, root, fset) {
		decls, queue := handlerRoots(files)
		bound := fieldBindings(files, decls)
		seen := make(map[*ast.FuncDecl]bool)
		for len(queue) > 0 {
			fn := queue[0]
			queue = queue[1:]
			if !seen[fn] {
				seen[fn] = true
				queue = append(queue, scanForAsk(fset, fn, decls, bound, &found)...)
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

// fieldBindings maps a field of a dependency struct (a composite literal of a type named *deps) to the package
// functions the literal stores in it (`fail: m.failLater`), so a handler calling a dependency field reaches the
// function wired into it.
func fieldBindings(files []*ast.File, decls map[string][]*ast.FuncDecl) map[string][]*ast.FuncDecl {
	bound := make(map[string][]*ast.FuncDecl)
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			lit, ok := node.(*ast.CompositeLit)
			if !ok || lit.Type == nil || !strings.HasSuffix(strings.ToLower(types.ExprString(lit.Type)), "deps") {
				return true
			}
			for _, elt := range lit.Elts {
				kv, isKV := elt.(*ast.KeyValueExpr)
				if !isKV {
					continue
				}
				if field, isIdent := kv.Key.(*ast.Ident); isIdent {
					bound[field.Name] = append(bound[field.Name], named(kv.Value, decls)...)
				}
			}
			return true
		})
	}
	return bound
}

// named are the package functions an expression names.
func named(expr ast.Expr, decls map[string][]*ast.FuncDecl) []*ast.FuncDecl {
	var out []*ast.FuncDecl
	ast.Inspect(expr, func(v ast.Node) bool {
		switch n := v.(type) {
		case *ast.SelectorExpr:
			out = append(out, decls[n.Sel.Name]...)
		case *ast.Ident:
			out = append(out, decls[n.Name]...)
		}
		return true
	})
	return out
}

// receiver is fn's receiver name and type name ("" for a function).
func receiver(fn *ast.FuncDecl) (string, string) {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return "", ""
	}
	field := fn.Recv.List[0]
	typ := field.Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	switch t := typ.(type) {
	case *ast.IndexExpr:
		typ = t.X
	case *ast.IndexListExpr:
		typ = t.X
	}
	name := ""
	if len(field.Names) > 0 {
		name = field.Names[0].Name
	}
	return name, types.ExprString(typ)
}

// methodsOf are the decls named name whose receiver type is typ.
func methodsOf(decls []*ast.FuncDecl, typ string) []*ast.FuncDecl {
	var out []*ast.FuncDecl
	for _, d := range decls {
		if _, t := receiver(d); t == typ {
			out = append(out, d)
		}
	}
	return out
}

// scanForAsk records every Ask selector in fn's body and returns the package functions fn names: a call or value on
// fn's receiver resolves to the receiver type's methods and the functions wired into its dependency fields, a
// selector on any other identifier (an import, a local, a parameter) to nothing, and a longer chain or a bare name
// to every package function of that name.
func scanForAsk(
	fset *token.FileSet,
	fn *ast.FuncDecl,
	decls, bound map[string][]*ast.FuncDecl,
	found *[]string,
) []*ast.FuncDecl {
	recvName, recvType := receiver(fn)
	var next []*ast.FuncDecl
	var visit func(ast.Node) bool
	visit = func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.SelectorExpr:
			if n.Sel.Name == "Ask" {
				*found = append(*found, fset.Position(n.Sel.Pos()).String()+": Ask in "+fn.Name.Name+
					", reachable from a keyed handler; send a message and handle the reply as one instead")
			}
			x, isIdent := n.X.(*ast.Ident)
			switch {
			case isIdent && recvName != "" && x.Name == recvName:
				next = append(next, methodsOf(decls[n.Sel.Name], recvType)...)
				next = append(next, bound[n.Sel.Name]...)
			case !isIdent:
				next = append(next, decls[n.Sel.Name]...)
				next = append(next, bound[n.Sel.Name]...)
				ast.Inspect(n.X, visit)
			}
			return false
		case *ast.Ident:
			next = append(next, decls[n.Name]...)
		}
		return true
	}
	ast.Inspect(fn.Body, visit)
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
