//go:build linux || (darwin && amd64)

package hanwen

import (
	"hash/fnv"
	"testing"
)

// Inode numbers must not change across releases: clients (and NFS re-exports
// of the mount) hold them across restarts. hashPath must stay FNV-1a 64.
func TestHashPathMatchesFNV1a(t *testing.T) {
	t.Parallel()
	for _, p := range []string{"", "/", "/__all__", "/__all__/Movie (2024)/movie.mkv", "/ünïcödé/名前"} {
		h := fnv.New64a()
		_, _ = h.Write([]byte(p))
		want := h.Sum64()
		if want <= 1 {
			want = 2
		}
		if got := hashPath(p); got != want {
			t.Errorf("hashPath(%q) = %d, want %d", p, got, want)
		}
	}
}
