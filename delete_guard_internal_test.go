package main

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestProviderTorrentDeletedOnlyByTheHolder keeps provider torrent deletion in one place: the holder in
// pkg/manager/holder_actor.go decides when a torrent is deleted, and pkg/debrid implements the call. A DeleteTorrent
// call or method value of common.Client's two-argument shape anywhere else skips the holder's recount.
func TestProviderTorrentDeletedOnlyByTheHolder(t *testing.T) {
	t.Parallel()
	for _, v := range providerDeleteCalls(t, ".") {
		t.Error(v)
	}
}

func TestProviderDeleteGuardCatchesAnInjectedViolation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSource(t, root, "pkg/manager/holder_actor.go", `package manager

func remove(c client) error { return c.DeleteTorrent(ctx, "id") }
`)
	writeSource(t, root, "pkg/debrid/providers/rd/rd.go", `package rd

func (r *RD) DeleteTorrent(ctx context.Context, id string) error { return r.inner.DeleteTorrent(ctx, id) }
`)
	writeSource(t, root, "pkg/manager/entry_delete.go", `package manager

func (m *Manager) DeleteTorrent(ctx context.Context, key string, all bool) error { return nil }

func caller(m *Manager) error { return m.DeleteTorrent(ctx, "key", true) }
`)
	writeSource(t, root, "pkg/manager/fake_test.go", `package manager

func (f *fake) DeleteTorrent(ctx context.Context, id string) error { return f.client.DeleteTorrent(ctx, id) }
`)
	writeSource(t, root, "pkg/manager/prune.go", `package manager

func prune(c client) error { return c.DeleteTorrent(ctx, "id") }

func later(c client) func(context.Context, string) error { return c.DeleteTorrent }
`)

	got := strings.Join(providerDeleteCalls(t, root), "\n")
	if strings.Count(got, "\n") != 1 || !strings.Contains(got, "prune.go:3") || !strings.Contains(got, "prune.go:5") {
		t.Errorf("provider delete guard = %q, want findings at prune.go:3 and prune.go:5 only", got)
	}
}

func providerDeleteCalls(t *testing.T, root string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var found []string
	for _, files := range governedPackages(t, root, fset) {
		for _, file := range files {
			path := filepath.ToSlash(fset.Position(file.Pos()).Filename)
			if strings.Contains(path, "pkg/debrid/") || strings.HasSuffix(path, "pkg/manager/holder_actor.go") {
				continue
			}
			found = append(found, deleteSelectors(fset, file)...)
		}
	}
	return found
}

func deleteSelectors(fset *token.FileSet, file *ast.File) []string {
	var found []string
	managerCalls := make(map[*ast.SelectorExpr]bool)
	ast.Inspect(file, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && len(call.Args) != 2 {
			if sel, isSel := call.Fun.(*ast.SelectorExpr); isSel {
				managerCalls[sel] = true
			}
		}
		return true
	})
	ast.Inspect(file, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if ok && sel.Sel.Name == "DeleteTorrent" && !managerCalls[sel] {
			found = append(found, fset.Position(sel.Pos()).String()+
				": DeleteTorrent on a provider client; release the torrent through the holder")
		}
		return true
	})
	return found
}
