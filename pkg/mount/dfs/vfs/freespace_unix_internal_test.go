//go:build !windows

package vfs

import (
	"math"
	"testing"
)

func TestAvailableBytes(t *testing.T) {
	t.Parallel()
	if got, err := availableBytes(10, int64(4096)); err != nil || got != 40960 {
		t.Fatalf("availableBytes(10, 4096) = %d, %v", got, err)
	}
	if got, err := availableBytes(10, uint32(512)); err != nil || got != 5120 {
		t.Fatalf("availableBytes(10, uint32 512) = %d, %v", got, err)
	}
	if _, err := availableBytes(10, int64(-1)); err == nil {
		t.Fatal("negative block size accepted")
	}
	if got, err := availableBytes(math.MaxUint64/2, int64(4)); err != nil || got != math.MaxUint64 {
		t.Fatalf("overflowing product = %d, %v; want saturation", got, err)
	}
	if got, err := availableBytes(10, int64(0)); err != nil || got != 0 {
		t.Fatalf("zero block size = %d, %v", got, err)
	}
}

func TestFreeDiskBytesReadsTempDir(t *testing.T) {
	t.Parallel()
	if _, err := freeDiskBytes(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
