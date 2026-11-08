package cgofuse

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"github.com/rs/zerolog"
	"github.com/winfsp/cgofuse/fuse"

	"github.com/sirrobot01/decypharr/internal/fsutil"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/backend"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/config"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/vfs"
	"github.com/sirrobot01/decypharr/pkg/mount/unmount"
)

// host is the part of *fuse.FileSystemHost the backend uses.
type host interface {
	// Mount blocks until the filesystem is unmounted; false means it failed.
	Mount(path string, opts []string) bool
	Unmount() bool
}

// mountDetacher lazily detaches a mount point (unmount.Unmounter).
type mountDetacher interface {
	Unmount(ctx context.Context, mountPath string) error
}

// Backend implements the cgofuse backend for cross-platform FUSE support.
type Backend struct {
	config    *config.FuseConfig
	logger    zerolog.Logger
	root      *FS
	newHost   func(*FS) host
	unmounter mountDetacher
}

// mounted is the live mount; done closes when the host's Mount returns.
type mounted struct {
	host     host
	done     chan struct{}
	path     string
	detacher mountDetacher
}

// Done is closed once when the serve loop has ended.
func (m *mounted) Done() <-chan struct{} { return m.done }

// Unmount asks the host to unmount; its Mount then returns.
func (m *mounted) Unmount() error {
	if !m.host.Unmount() {
		return errors.New("cgofuse unmount failed")
	}
	return nil
}

// Detach lazily detaches the mount point (cgofuse has no lazy mode of its own).
func (m *mounted) Detach(ctx context.Context) error { return m.detacher.Unmount(ctx, m.path) }

// NewBackend creates a new cgofuse backend.
func NewBackend(vfs *vfs.Manager, config *config.FuseConfig, log zerolog.Logger) (backend.Backend, error) {
	return &Backend{
		config: config,
		logger: log,
		root:   NewFS(vfs, config, log),
		newHost: func(f *FS) host {
			h := fuse.NewFileSystemHost(f)
			h.SetCapReaddirPlus(true)     // Enable ReaddirPlus (used by WinFsp on Windows)
			h.SetCapCaseInsensitive(true) // Windows is case-insensitive
			return h
		},
		unmounter: unmount.New(log),
	}, nil
}

// mountOptions builds the host's command line options for this platform.
func mountOptions() []string {
	// Common options
	options := []string{"-o", "fsname=dfs"}

	// Platform-specific options
	switch runtime.GOOS {
	case "windows":
		// WinFsp options
		options = append(options, "-o", "uid=-1,gid=-1")
		options = append(options, "-o", "volname=DFS")
		options = append(options, "--FileSystemName=DFS")
	case "darwin":
		// macFUSE options
		options = append(options, "-o", "volname=dfs")
		options = append(options, "-o", "noapplexattr")
		options = append(options, "-o", "noappledouble")
		options = append(options, "-o", "allow_other")
	default:
		// Linux FUSE options
		options = append(options, "-o", "allow_other")
		options = append(options, "-o", "default_permissions")
	}
	return options
}

// Mount mounts the filesystem using cgofuse: the blocking host.Mount runs as
// task "serve" on g, and the mount is ready when the host calls Init.
func (b *Backend) Mount(ctx context.Context, g *task.Group) (backend.Mounted, error) {
	// Create mount point if it doesn't exist (skip on Windows)
	if runtime.GOOS != "windows" {
		_ = fsutil.MkdirShared(b.config.MountPath, b.config.MountDirMode)
	}
	if b.root == nil {
		return nil, fmt.Errorf("root node is not initialized")
	}

	b.root.ctx = g.Context()
	h := b.newHost(b.root)
	handle := &mounted{host: h, done: make(chan struct{}), path: b.config.MountPath, detacher: b.unmounter}
	options := mountOptions()

	served := make(chan bool, 1)
	if err := g.Go("serve", func(context.Context) error {
		// Mount returns when unmounted
		ok := h.Mount(b.config.MountPath, options)
		close(handle.done)
		served <- ok
		return nil
	}); err != nil {
		return nil, fmt.Errorf("start cgofuse serve: %w", err)
	}

	mountCtx, cancel := context.WithTimeout(ctx, b.config.DaemonTimeout)
	defer cancel()
	select {
	case <-b.root.initialized:
		return handle, nil
	case <-served:
		return nil, errors.New("cgofuse mount failed")
	case <-mountCtx.Done():
		h.Unmount()
		return nil, fmt.Errorf("waiting for cgofuse init: %w", mountCtx.Err())
	}
}

// Type returns the backend type.
func (b *Backend) Type() backend.Type {
	return backend.Cgo
}

func (b *Backend) Refresh(_ string) {
}
