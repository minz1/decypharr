package backend

import (
	"context"
	"os"
	"runtime"

	"github.com/sirrobot01/decypharr/internal/task"
)

// Type represents the type of FUSE backend.
type Type string

const (
	Hanwen Type = "hanwen"
	Cgo    Type = "cgo"
)

// Mounted is a live mount. The dfs owner task holds it from Mount to unmount.
type Mounted interface {
	// Done is closed once when the serve loop has ended.
	Done() <-chan struct{}
	// Unmount is the graceful unmount; it may block.
	Unmount() error
	// Detach is the lazy detach of the mount point, bounded by ctx.
	Detach(ctx context.Context) error
}

// Backend represents a FUSE backend implementation.
type Backend interface {
	// Mount mounts the filesystem at the configured path and returns once it
	// is ready to serve. Tasks the backend needs run on g.
	Mount(ctx context.Context, g *task.Group) (Mounted, error)

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
