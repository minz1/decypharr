package manager

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func libEntry(key, name string) *storage.Entry {
	e := testEntry(key)
	e.Name = name
	e.Files = map[string]*storage.File{"f.mkv": {Name: "f.mkv", Size: 1, InfoHash: key}}
	return e
}

func (h *actorHarness) libManager() *Manager {
	m := h.manager()
	m.work = h.root
	return m
}

func (h *actorHarness) grabbed() {
	h.t.Helper()
	const key = "a"
	seedPhase(h.t, h.store, key, grab.PhaseComplete)
	_ = h.ask(key, eventMsg(grab.Event{Kind: grab.EvRecover}))
	if _, ok := h.grabs.get(key); !ok {
		h.t.Fatalf("%s has no grab", key)
	}
}

func storedName(t *testing.T, store *storage.Storage, key string) string {
	t.Helper()
	e, err := store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	return e.Name
}

func TestSyncWriteOfAGrabKeyGoesThroughTheActor(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		h.grabbed()
		if err := store.AddOrUpdate(libEntry("a", "old")); err != nil {
			t.Fatal(err)
		}
		m := h.libManager()

		put := libraryOp{kind: libPut, entry: libEntry("a", "synced")}
		if err := m.writeLibrary(t.Context(), "a", put); err != nil {
			t.Fatal(err)
		}
		if got := storedName(t, store, "a"); got != "synced" {
			t.Fatalf("name %q after the write, want synced", got)
		}

		h.stop()
		put = libraryOp{kind: libPut, entry: libEntry("a", "direct")}
		if err := m.writeLibrary(t.Context(), "a", put); !errors.Is(err, keyed.ErrStopped) {
			t.Fatalf("write with the actor stopped: %v, want ErrStopped", err)
		}
		if got := storedName(t, store, "a"); got != "synced" {
			t.Fatalf("name %q: a grab key was written without its actor", got)
		}
	})
}

func TestSyncWriteOfAnUngrabbedKeyIsDirect(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		m := h.libManager()
		h.stop()

		put := libraryOp{kind: libPut, entry: libEntry("b", "synced")}
		if err := m.writeLibrary(t.Context(), "b", put); err != nil {
			t.Fatalf("write of a key with no grab: %v", err)
		}
		if got := storedName(t, store, "b"); got != "synced" {
			t.Fatalf("name %q, want synced", got)
		}
	})
}

func TestCascadeDeleteOfAGrabKeyRemovesTheGrabFirst(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		h.grabbed()
		if err := store.AddOrUpdate(libEntry("a", "old")); err != nil {
			t.Fatal(err)
		}

		op := libraryOp{kind: libDelete, cascade: true}
		if err := h.libManager().writeLibrary(t.Context(), "a", op); err != nil {
			t.Fatal(err)
		}
		if got := h.phase("a"); got != grab.PhaseDeleting {
			t.Fatalf("grab phase %v after the delete, want deleting", got)
		}
		h.next(grab.ChildCleanup)
		if _, err := store.Get("a"); !errors.Is(err, appendstore.ErrKeyNotFound) {
			t.Fatalf("library entry after the delete: %v, want not found", err)
		}
	})
}

func TestProviderDropKeepsTheFailedRecord(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		h.grabbed()
		if err := store.AddOrUpdate(libEntry("a", "old")); err != nil {
			t.Fatal(err)
		}

		if err := h.libManager().dropLibraryEntry(t.Context(), "a"); err != nil {
			t.Fatal(err)
		}
		if got := h.phase("a"); got != grab.PhaseFailed {
			t.Fatalf("grab phase %v after the drop, want failed", got)
		}
		if _, err := store.GetQueuedGrab("a"); err != nil {
			t.Fatalf("the failed record is gone: %v", err)
		}
		if _, err := store.Get("a"); !errors.Is(err, appendstore.ErrKeyNotFound) {
			t.Fatalf("library entry after the drop: %v, want not found", err)
		}
	})
}

func TestReAddNeverClearsBad(t *testing.T) {
	t.Parallel()
	bad := func(t *testing.T, store *storage.Storage) {
		t.Helper()
		e := libEntry("a", "bad")
		e.Bad = true
		if err := store.AddOrUpdate(e); err != nil {
			t.Fatal(err)
		}
	}
	mustBeBad := func(t *testing.T, store *storage.Storage) {
		t.Helper()
		e, err := store.Get("a")
		if err != nil {
			t.Fatal(err)
		}
		if !e.Bad {
			t.Fatal("the library entry lost its bad mark")
		}
	}
	t.Run("submit", func(t *testing.T) {
		t.Parallel()
		store := newTestStore(t)
		synctest.Test(t, func(t *testing.T) {
			h := newActorHarness(t, store)
			bad(t, store)
			if err := h.libManager().submitEntry(t.Context(), testEntry("a"), addReq()); err != nil {
				t.Fatal(err)
			}
			h.next(grab.ChildSubmit)
			mustBeBad(t, store)
		})
	})
	t.Run("publish", func(t *testing.T) {
		t.Parallel()
		store := newTestStore(t)
		synctest.Test(t, func(t *testing.T) {
			h := newActorHarness(t, store)
			bad(t, store)
			seedPhase(t, store, "a", grab.PhaseProvider)
			h.mustAsk("a", grab.Event{Kind: grab.EvRecover})
			h.next(grab.ChildPoll)
			h.mustAsk("a", grab.Event{Kind: grab.EvProviderObserved, Downloaded: true, AllLinked: true})
			h.next(grab.ChildAction)
			mustBeBad(t, store)
		})
	})
}

func TestBatchWithTwoKeysOnOneStripe(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		h := newActorHarness(t, store)
		h.grabbed()
		if err := store.AddOrUpdate(libEntry("a", "old")); err != nil {
			t.Fatal(err)
		}
		const keys = 130
		batch := []*storage.Entry{libEntry("a", "synced")}
		for i := range keys {
			batch = append(batch, libEntry(fmt.Sprintf("k%d", i), "synced"))
		}

		if err := h.libManager().batchWriteLibrary(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
		for _, e := range batch {
			if got := storedName(t, store, e.InfoHash); got != "synced" {
				t.Fatalf("%s: name %q, want synced", e.InfoHash, got)
			}
		}
	})
}

func isLibraryWrite(name string) bool {
	return slices.Contains([]string{"AddOrUpdate", "BatchAddOrUpdate", "Delete", "UpdateItem"}, name)
}

func isLibraryWriter(name string) bool {
	return slices.Contains([]string{"applyLibraryOp", "library", "batchWriteLibrary", "pruneDelete"}, name)
}

// libraryWriteOn names the call a library write on a storage or store field makes ("" for any other call).
func libraryWriteOn(call *ast.CallExpr) (string, token.Pos) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !isLibraryWrite(sel.Sel.Name) {
		return "", 0
	}
	recv := ""
	switch x := sel.X.(type) {
	case *ast.SelectorExpr:
		recv = x.Sel.Name
	case *ast.Ident:
		recv = x.Name
	}
	if recv != "storage" && recv != "store" {
		return "", 0
	}
	return recv + "." + sel.Sel.Name, sel.Sel.Pos()
}

// directLibraryWrites reports calls of a library write on a storage or store field outside the exempt functions.
func directLibraryWrites(fset *token.FileSet, files []*ast.File) []string {
	var found []string
	for _, file := range files {
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || isLibraryWriter(fn.Name.Name) {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if call, isCall := node.(*ast.CallExpr); isCall {
					if name, pos := libraryWriteOn(call); name != "" {
						found = append(found, fset.Position(pos).String()+": "+name+" in "+fn.Name.Name+
							"; write the library through writeLibrary")
					}
				}
				return true
			})
		}
	}
	return found
}

func parseDir(t *testing.T, dir string) (*token.FileSet, []*ast.File) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		src, readErr := os.ReadFile(p)
		if readErr != nil {
			t.Fatal(readErr)
		}
		f, parseErr := parser.ParseFile(fset, p, src, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		files = append(files, f)
	}
	return fset, files
}

func TestNoDirectLibraryWritesInManager(t *testing.T) {
	t.Parallel()
	fset, files := parseDir(t, ".")
	for _, v := range directLibraryWrites(fset, files) {
		t.Error(v)
	}

	fset = token.NewFileSet()
	src := `package manager

func (m *Manager) bad() { _ = m.storage.AddOrUpdate(nil) }

func (m *Manager) pruneDelete() { _ = m.storage.Delete("") }

func (m *Manager) fine() { _ = m.storage.Get(""); _ = m.queue.Delete("") }
`
	f, err := parser.ParseFile(fset, "x.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	if got := directLibraryWrites(fset, []*ast.File{f}); len(got) != 1 || !strings.Contains(got[0], "in bad") {
		t.Fatalf("guard findings %q, want one in bad", got)
	}
}
