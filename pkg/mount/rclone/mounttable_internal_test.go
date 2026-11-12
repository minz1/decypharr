package rclone

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestFuseEntriesAt(t *testing.T) {
	t.Parallel()
	const (
		root    = "22 1 0:21 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n"
		ours    = "40 22 0:40 / /mnt/media rw,nosuid,nodev,relatime shared:20 - fuse.rclone dav: rw,user_id=0\n"
		stacked = "46 40 0:46 / /mnt/media rw,nosuid,nodev,relatime shared:26 - fuse.rclone dav: rw,user_id=0\n"
		spaced  = "41 22 0:41 / /mnt/my\\040media rw,relatime shared:21 - fuse.rclone dav: rw\n"
		slashed = "42 22 0:42 / /mnt/back\\134slash rw,relatime shared:22 - fuse dav: rw\n"
		ext4At  = "43 22 0:43 / /mnt/media rw,relatime shared:23 - ext4 /dev/sdb1 rw\n"
		other   = "44 22 0:44 / /mnt/other rw,relatime shared:24 - fuse.rclone dav: rw\n"
		bad     = "garbage line\n"
		noSep   = "45 22 0:45 / /mnt/media rw,relatime fuse.rclone dav: rw\n"
		badID   = "x 22 0:47 / /mnt/media rw,relatime shared:27 - fuse.rclone dav: rw\n"
	)
	cases := []struct {
		name  string
		table string
		path  string
		want  []tableEntry
	}{
		{"plain", root + ours, "/mnt/media", []tableEntry{{40, 22}}},
		{"space escape", root + spaced, "/mnt/my media", []tableEntry{{41, 22}}},
		{"backslash escape", root + slashed, `/mnt/back\slash`, []tableEntry{{42, 22}}},
		{"trailing slash given", root + ours, "/mnt/media/", []tableEntry{{40, 22}}},
		{"not fuse", root + ext4At, "/mnt/media", nil},
		{"other path", root + other, "/mnt/media", nil},
		{"stacked", root + ours + stacked, "/mnt/media", []tableEntry{{40, 22}, {46, 40}}},
		{"malformed skipped", bad + root + noSep + badID + ours, "/mnt/media", []tableEntry{{40, 22}}},
		{"empty", "", "/mnt/media", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := fuseEntriesAt([]byte(tc.table), tc.path); !slices.Equal(got, tc.want) {
				t.Fatalf("fuseEntriesAt(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestTopEntry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		entries []tableEntry
		want    int
		ok      bool
	}{
		{"none", nil, 0, false},
		{"one", []tableEntry{{40, 22}}, 40, true},
		{"stacked", []tableEntry{{40, 22}, {46, 40}}, 46, true},
		{"stacked out of order", []tableEntry{{46, 40}, {40, 22}}, 46, true},
		{"cycle", []tableEntry{{1, 2}, {2, 1}}, 0, false},
		{"two uncovered, last wins", []tableEntry{{40, 22}, {41, 22}}, 41, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, ok := topEntry(tc.entries); got != tc.want || ok != tc.ok {
				t.Fatalf("topEntry(%v) = %d, %v, want %d, %v", tc.entries, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestUnescapeMountinfo(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		`/a\040b`:   "/a b",
		`/a\011b`:   "/a\tb",
		`/a\012b`:   "/a\nb",
		`/a\134b`:   `/a\b`,
		`/plain`:    "/plain",
		`/odd\04`:   `/odd\04`,
		`/notoct\9`: `/notoct\9`,
	}
	for in, want := range cases {
		if got := unescapeMountinfo(in); got != want {
			t.Errorf("unescapeMountinfo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTablePathResolvesParentSymlink(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(dir, "realDir")
	if err = os.Mkdir(realDir, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err = os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	if got, want := tablePathFor(filepath.Join(link, "mnt")), filepath.Join(realDir, "mnt"); got != want {
		t.Fatalf("tablePathFor via symlink = %q, want %q", got, want)
	}
	// The base name is never resolved, even when it is itself a symlink.
	if got, want := tablePathFor(link), link; got != want {
		t.Fatalf("tablePathFor(symlink mount point) = %q, want %q", got, want)
	}
}

func TestTablePathMissingParentFallsBack(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "nope", "deeper", "mnt") + "/"
	want := filepath.Clean(missing)
	if got := tablePathFor(missing); got != want {
		t.Fatalf("tablePathFor(missing parent) = %q, want %q", got, want)
	}
}

func TestTablePathRelativeIsAbsolute(t *testing.T) {
	t.Parallel()
	if got := tablePathFor("relative/mnt"); !filepath.IsAbs(got) {
		t.Fatalf("tablePathFor(relative) = %q, want an absolute path", got)
	}
}
