//go:build linux || (darwin && amd64)

package hanwen

import "time"

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

// unixSeconds is t as FUSE attribute seconds; times before the epoch clamp
// to 0 instead of wrapping to a far-future uint64.
func unixSeconds(t time.Time) uint64 {
	return nonNegative(t.Unix())
}

// nonNegative converts v for FUSE's unsigned attribute fields, clamping
// negatives to 0.
func nonNegative(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}
