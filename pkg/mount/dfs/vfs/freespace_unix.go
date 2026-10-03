//go:build !windows

package vfs

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

// freeDiskBytes reports the bytes available to unprivileged users on the
// filesystem holding path.
func freeDiskBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return availableBytes(st.Bavail, st.Bsize)
}

// availableBytes multiplies free blocks by the block size, whose type varies
// by platform (int64 on linux, uint32 on darwin). A negative size is an
// error, and a product beyond uint64 saturates.
func availableBytes[S ~int32 | ~int64 | ~uint32 | ~uint64](blocks uint64, size S) (uint64, error) {
	if size < 0 {
		return 0, fmt.Errorf("statfs reported a negative block size %d", size)
	}
	blockSize := uint64(size)
	if blockSize != 0 && blocks > math.MaxUint64/blockSize {
		return math.MaxUint64, nil
	}
	return blocks * blockSize, nil
}
