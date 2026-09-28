package backend

import (
	"context"
	"os"
	"runtime"
)

// Type represents the type of FUSE backend.
type Type string

const (
	Hanwen Type = "hanwen"
	Cgo    Type = "cgo"
)

// Backend represents a FUSE backend implementation.
type Backend interface {
	// Mount mounts the filesystem at the configured path
	Mount(ctx context.Context) error

	// Unmount unmounts the filesystem
	Unmount(ctx context.Context) error

	// WaitReady waits for the mount to be ready
	WaitReady(ctx context.Context) error

	// IsReady returns true if the mount is ready
	IsReady() bool

	Refresh(dir string)

	// Type returns the backend type
	Type() Type
}

// GetDefaultBackendType returns the recommended backend for the current platform
// Linux: hanwen (fastest, pure Go)
// macOS/Windows: cgofuse (cross-platform, works with Fuse-T/WinFsp).
func GetDefaultBackendType() Type {
	if runtime.GOOS == "linux" && os.Getenv("DFS_FUSE_BACKEND") != "cgo" {
		return Hanwen
	}
	return Cgo
}
