package manager

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

const testMount = "/mnt/decypharr"

func linkAt(t *testing.T, dir, name, target string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func lookupMap(m map[[2]string]string) func(string, string) (string, bool, error) {
	return func(folder, file string) (string, bool, error) {
		h, ok := m[[2]string{folder, file}]
		return h, ok, nil
	}
}

func TestScanResolvesPerFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	linkAt(t, root, "a", testMount+"/__all__/Show/a.mkv")
	linkAt(t, root, "b", testMount+"/__all__/Show/b.mkv")
	lookup := lookupMap(map[[2]string]string{{"Show", "a.mkv"}: "new", {"Show", "b.mkv"}: "new"})

	got, err := scanReferences([]string{root}, testMount, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if got.Links != 2 || len(got.Referenced) != 1 {
		t.Fatalf("got %+v", got)
	}
	if _, ok := got.Referenced["new"]; !ok {
		t.Fatalf("new not referenced: %+v", got)
	}
	if _, ok := got.Referenced["old"]; ok {
		t.Fatal("shadowed entry referenced")
	}
}

func TestScanIgnoresLinksOutsideMount(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	linkAt(t, root, "x", "/etc/hostname")
	got, err := scanReferences([]string{root}, testMount, lookupMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.Links != 1 || len(got.Referenced) != 0 || got.Dangling != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestScanCountsDangling(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	linkAt(t, root, "x", testMount+"/__all__/Show/a.mkv")
	got, err := scanReferences([]string{root}, testMount, lookupMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.Dangling != 1 || got.Links != 1 || len(got.Referenced) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestScanAbortsOnUnparseableMountLink(t *testing.T) {
	t.Parallel()
	for name, target := range map[string]string{
		"dir link":    testMount + "/__all__/Show",
		"other group": testMount + "/torbox/Show/a.mkv",
		"too deep":    testMount + "/__all__/Show/sub/a.mkv",
		"mount root":  testMount,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			p := linkAt(t, root, "bad", target)
			_, err := scanReferences([]string{root}, testMount, lookupMap(nil))
			if err == nil || !strings.Contains(err.Error(), p) {
				t.Fatalf("want error naming %s, got %v", p, err)
			}
		})
	}
}

func TestScanAbortsOnUnreadableRoot(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := scanReferences([]string{missing}, testMount, lookupMap(nil)); err == nil {
		t.Fatal("missing root: want error")
	}
}

func TestScanAbortsOnUnreadableSubdir(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	sub := filepath.Join(root, "locked")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o750) })
	if _, err := scanReferences([]string{root}, testMount, lookupMap(nil)); err == nil {
		t.Fatal("want error")
	}
}

func TestScanCleansTargets(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	lib := filepath.Join(root, "lib", "deep")
	linkAt(t, lib, "dirty", testMount+"//__all__/./Show/a.mkv")
	linkAt(t, lib, "slash", testMount+"/__all__/Show/b.mkv/")
	rel, err := filepath.Rel(lib, testMount+"/__all__/Show/c.mkv")
	if err != nil {
		t.Fatal(err)
	}
	linkAt(t, lib, "rel", rel)
	lookup := lookupMap(map[[2]string]string{
		{"Show", "a.mkv"}: "ha", {"Show", "b.mkv"}: "hb", {"Show", "c.mkv"}: "hc",
	})
	got, err := scanReferences([]string{root}, testMount, lookup)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct{}{"ha": {}, "hb": {}, "hc": {}}
	if !maps.Equal(got.Referenced, want) || got.Links != 3 {
		t.Fatalf("got %+v", got)
	}
}

func TestScanByteExactNames(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	f1, n1 := "The.Twilight.Saga.Eclipse.2010-Нечипорук", "[D4C] JoJo's (2012) % 1.mkv"
	linkAt(t, root, "a", testMount+"/__all__/"+f1+"/movie.mkv")
	linkAt(t, root, "b", testMount+"/__all__/Show [x] 100%/"+n1)
	lookup := lookupMap(map[[2]string]string{{f1, "movie.mkv"}: "h1", {"Show [x] 100%", n1}: "h2"})
	got, err := scanReferences([]string{root}, testMount, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Referenced) != 2 || got.Dangling != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestLookupServedEntryUsesNewestFile(t *testing.T) {
	t.Parallel()
	store, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now()
	mk := func(hash string, added time.Time, size int64) *storage.Entry {
		return &storage.Entry{
			InfoHash: hash, Name: "Show",
			Files: map[string]*storage.File{"a.mkv": {Name: "a.mkv", InfoHash: hash, AddedOn: added, Size: size}},
		}
	}
	for _, e := range []*storage.Entry{mk("old", now.Add(-time.Hour), 1), mk("new", now, 2)} {
		if addErr := store.AddOrUpdate(e); addErr != nil {
			t.Fatal(addErr)
		}
	}
	m := &Manager{storage: store}
	folder := (&storage.Entry{InfoHash: "new", Name: "Show"}).GetFolder("")
	if h, ok, lerr := m.lookupServedEntry(folder, "a.mkv"); lerr != nil || !ok || h != "new" {
		t.Fatalf("got %q %v %v, want new", h, ok, lerr)
	}
	if _, ok, lerr := m.lookupServedEntry(folder, "missing.mkv"); ok || lerr != nil {
		t.Fatalf("missing file: %v %v", ok, lerr)
	}
	if _, ok, lerr := m.lookupServedEntry("nope", "a.mkv"); ok || lerr != nil {
		t.Fatalf("missing folder: %v %v", ok, lerr)
	}
	item, err := store.GetEntryItem(folder)
	if err != nil {
		t.Fatal(err)
	}
	item.Files["a.mkv"].Deleted = true
	if updateErr := store.UpdateItem(item); updateErr != nil {
		t.Fatal(updateErr)
	}
	if _, ok, lerr := m.lookupServedEntry(folder, "a.mkv"); ok || lerr != nil {
		t.Fatalf("deleted file: %v %v", ok, lerr)
	}
	if closeErr := store.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if _, _, lerr := m.lookupServedEntry(folder, "a.mkv"); lerr == nil {
		t.Fatal("closed store: want error, got not-found semantics")
	}
}

func TestScanAbortsOnLookupError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	linkAt(t, root, "x", testMount+"/__all__/Show/a.mkv")
	boom := errors.New("boom")
	_, err := scanReferences([]string{root}, testMount, func(string, string) (string, bool, error) {
		return "", false, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
}

func TestScanFollowsSymlinkedRoot(t *testing.T) {
	t.Parallel()
	physical := t.TempDir()
	linkAt(t, physical, "a", testMount+"/__all__/Show/a.mkv")
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	got, err := scanReferences([]string{alias}, testMount, lookupMap(map[[2]string]string{{"Show", "a.mkv"}: "h"}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Links != 1 || len(got.Referenced) != 1 {
		t.Fatalf("got %+v", got)
	}
}

func chain(t *testing.T, dir string, n int, final string) string {
	t.Helper()
	next := final
	for i := n; i >= 1; i-- {
		next = linkAt(t, dir, fmt.Sprintf("hop%d", i), next)
	}
	return next
}

func TestScanFollowsChainedLinks(t *testing.T) {
	t.Parallel()
	lookup := lookupMap(map[[2]string]string{{"Show", "a.mkv"}: "h"})
	mounted := testMount + "/__all__/Show/a.mkv"

	root, mid := t.TempDir(), t.TempDir()
	linkAt(t, root, "lib", chain(t, mid, 3, mounted))
	got, err := scanReferences([]string{root}, testMount, lookup)
	if err != nil || len(got.Referenced) != 1 {
		t.Fatalf("chain of 3: %+v %v", got, err)
	}

	root, mid = t.TempDir(), t.TempDir()
	linkAt(t, root, "lib", chain(t, mid, 9, mounted))
	if _, err = scanReferences([]string{root}, testMount, lookup); err == nil {
		t.Fatal("chain of 9: want error")
	}

	root, mid = t.TempDir(), t.TempDir()
	a, b := filepath.Join(mid, "a"), filepath.Join(mid, "b")
	linkAt(t, mid, "a", b)
	linkAt(t, mid, "b", a)
	linkAt(t, root, "lib", a)
	if _, err = scanReferences([]string{root}, testMount, lookup); err == nil {
		t.Fatal("loop: want error")
	}

	root = t.TempDir()
	linkAt(t, root, "gone", filepath.Join(mid, "nonexistent"))
	if got, err = scanReferences([]string{root}, testMount, lookup); err != nil || got.Links != 1 {
		t.Fatalf("dangling outside: %+v %v", got, err)
	}
}

func TestScanRejectsBadMountPath(t *testing.T) {
	t.Parallel()
	for _, mp := range []string{"", ".", "/", "relative/mount"} {
		if _, err := scanReferences(nil, mp, lookupMap(nil)); err == nil {
			t.Fatalf("mount %q: want error", mp)
		}
	}
}

func TestScanNormalizesMountPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	linkAt(t, root, "a", "/srv/fuse/__all__/Show/a.mkv")
	linkAt(t, root, "b", "/srv/fuse-other/__all__/Show/a.mkv")
	got, err := scanReferences([]string{root}, "/srv/fuse/", lookupMap(map[[2]string]string{{"Show", "a.mkv"}: "h"}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Links != 2 || len(got.Referenced) != 1 || got.Dangling != 0 {
		t.Fatalf("got %+v", got)
	}
}
