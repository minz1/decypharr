//go:build !windows

package vfs

import "golang.org/x/sys/unix"

// freeDiskBytes reports the bytes available to unprivileged users on the
// filesystem holding path.
func freeDiskBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	// Bsize is int64 on linux and uint32 on darwin.
	return st.Bavail * uint64(st.Bsize), nil //nolint:gosec // G115: statfs block size is never negative
}
