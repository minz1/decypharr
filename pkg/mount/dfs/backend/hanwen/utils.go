//go:build linux || (darwin && amd64)

package hanwen

// FNV-1a 64-bit parameters (see hash/fnv).
const (
	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
)

// hashPath returns the FNV-1a hash of path, the stable inode number for a
// virtual path. Inlined rather than via hash/fnv so the hot lookup path does
// not allocate a hasher. 0 and 1 are reserved by go-fuse, so they map to 2.
func hashPath(path string) uint64 {
	h := uint64(fnvOffset64)
	for i := range len(path) {
		h ^= uint64(path[i])
		h *= fnvPrime64
	}
	if h <= 1 {
		h = 2
	}
	return h
}
