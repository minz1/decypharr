//go:build windows

package vfs

import "golang.org/x/sys/windows"

// freeDiskBytes reports the bytes available to the calling user on the volume
// holding path.
func freeDiskBytes(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, nil, nil); err != nil {
		return 0, err
	}
	return free, nil
}
