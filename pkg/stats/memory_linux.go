//go:build linux

package stats

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// processRSS reads the kernel's resident page estimate without scanning mappings.
func processRSS() (uint64, error) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	// statm fields: size resident shared text lib data dt (in pages).
	const residentField = 1
	if len(fields) <= residentField {
		return 0, fmt.Errorf("missing resident page count in /proc/self/statm")
	}
	pages, err := strconv.ParseUint(fields[residentField], 10, 64)
	if err != nil {
		return 0, err
	}
	pageSize := os.Getpagesize()
	if pageSize <= 0 {
		return 0, fmt.Errorf("invalid page size %d", pageSize)
	}
	return pages * uint64(pageSize), nil
}
