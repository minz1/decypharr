//go:build linux

package buffer

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// prepareSparse is a no-op: Linux files are sparse by default.
func prepareSparse(*os.File) error { return nil }

// punchHole deallocates [offset, offset+length). KEEP_SIZE preserves the
// logical size so fixed per-offset write addresses stay valid. Partial blocks
// at the edges are zeroed, so the whole range is freed of data.
func punchHole(f *os.File, offset, length int64) (Range, error) {
	if f == nil || length <= 0 {
		return Range{}, nil
	}
	sc, err := f.SyscallConn()
	if err != nil {
		return Range{}, err
	}
	var opErr error
	if controlErr := sc.Control(func(fd uintptr) {
		opErr = unix.Fallocate(int(fd),
			unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, offset, length)
	}); controlErr != nil {
		return Range{}, controlErr
	}
	if errors.Is(opErr, unix.EOPNOTSUPP) || errors.Is(opErr, unix.ENOTSUP) {
		return Range{}, errPunchUnsupported
	}
	if opErr != nil {
		return Range{}, opErr
	}
	return Range{Off: offset, Size: length}, nil
}
