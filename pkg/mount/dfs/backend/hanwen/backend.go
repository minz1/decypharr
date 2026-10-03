//go:build linux || (darwin && amd64)

package hanwen

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/fsutil"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/backend"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/config"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/vfs"
	"github.com/sirrobot01/decypharr/pkg/mount/unmount"
)

const (
	// ReadTimeout is the maximum time a single read operation can take
	// Increased to 120s to handle slow debrid/CDN connections.
	ReadTimeout  = 120 * time.Second
	AttrTimeout  = 30 * time.Second
	EntryTimeout = 1 * time.Second

	// maxWrite is the largest FUSE request payload negotiated with the kernel.
	maxWrite = 1 << 20
	dirPerm  = 0o755
	filePerm = 0o644
	// statBlockSize is the unit of the Blocks attribute.
	statBlockSize = 512
)

// Backend implements the hanwen/go-fuse backend.
type Backend struct {
	config      *config.FuseConfig
	logger      zerolog.Logger
	server      *fuse.Server
	ready       atomic.Bool
	unmountFunc func(ctx context.Context)
	root        *Dir
	vfs         *vfs.Manager
	unmounter   mountDetacher
}

// mountDetacher force-detaches a mount point (unmount.Unmounter).
type mountDetacher interface {
	Unmount(ctx context.Context, mountPath string) error
}

// fuseServer is the part of *fuse.Server teardown uses.
type fuseServer interface {
	Unmount() error
}

// NewBackend creates a new hanwen backend.
func NewBackend(vfs *vfs.Manager, config *config.FuseConfig, log zerolog.Logger) (backend.Backend, error) {
	now := time.Now()
	// One shared rate-limited logger for the whole mount. Files/Dirs reference
	// it instead of allocating their own xsync map per inode — dedup keys are
	// already unique per inode so a shared map gives identical behaviour.
	rl := logger.NewRateLimitedLogger(logger.WithLogger(log))
	root := NewDir(vfs, "", LevelRoot, unixSeconds(now), config, log, rl)
	return &Backend{
		config:    config,
		logger:    log,
		root:      root,
		vfs:       vfs,
		unmounter: unmount.New(log),
	}, nil
}

// Mount mounts the filesystem using hanwen/go-fuse.
func (b *Backend) Mount(ctx context.Context) error {
	// Create mount point if it doesn't exist(skip if on Windows)

	if b.root == nil {
		return fmt.Errorf("root node is not initialized")
	}
	if b.vfs == nil {
		return fmt.Errorf("VFS manager is not initialized")
	}

	_ = fsutil.MkdirShared(b.config.MountPath, b.config.MountDirMode)
	// Try to unmount if already mounted
	b.forceUnmount(ctx)

	opts := b.mountOptions()

	// Start timer before creating NodeFS - adjust timeout duration as needed
	mountCtx, cancel := context.WithTimeout(ctx, b.config.DaemonTimeout)
	defer cancel()

	// Channel to receive the result of fs.Mount
	type fsResult struct {
		server *fuse.Server
		err    error
	}
	fsResultChan := make(chan fsResult, 1)

	// Run fs.Mount in a goroutine
	go func() {
		server, err := fs.Mount(b.config.MountPath, b.root, opts)
		fsResultChan <- fsResult{server: server, err: err}
	}()

	var server *fuse.Server
	select {
	case result := <-fsResultChan:
		server = result.server
		if result.err != nil {
			return fmt.Errorf("failed to create mount: %w", result.err)
		}
	case <-mountCtx.Done():
		b.ready.Store(false)
		// If fs.Mount later succeeds, nobody is left to read the result —
		// unmount the orphaned server instead of leaking a live mount.
		go func() {
			if result := <-fsResultChan; result.server != nil {
				_ = result.server.Unmount()
			}
		}()
		return fmt.Errorf("timeout creating mount: %w", mountCtx.Err())
	}

	b.server = server

	// Now wait for the mount to be ready with the same timeout context
	b.logger.Info().
		Str("mount_path", b.config.MountPath).
		Msg("Waiting for mount to be ready")

	waitChan := make(chan error, 1)
	go func() {
		waitChan <- server.WaitMount()
	}()

	select {
	case err := <-waitChan:
		if err != nil {
			_ = server.Unmount() // cleanup on error
			return fmt.Errorf("failed to wait for mount: %w", err)
		}
	case <-mountCtx.Done():
		_ = server.Unmount() // cleanup on timeout
		return fmt.Errorf("timeout waiting for mount to be ready: %w", mountCtx.Err())
	}

	b.unmountFunc = func(ctx context.Context) { b.unmountServer(ctx, server) }
	b.ready.Store(true)
	return nil
}

// unmountServer closes the VFS manager and unmounts server, force-unmounting
// when the regular unmount fails or ctx expires first.
func (b *Backend) unmountServer(ctx context.Context, server fuseServer) {
	b.logger.Info().Msg("Unmounting filesystem")

	done := make(chan error, 1)
	go func() {
		if b.vfs != nil {
			if err := b.vfs.Close(); err != nil {
				b.logger.Warn().Err(err).Msg("Failed to close VFS")
			}
		}
		// Unmount returns once the kernel has detached the mount and the
		// serve loop has exited, or with the reason it could not.
		done <- server.Unmount()
	}()

	select {
	case err := <-done:
		if err == nil {
			b.logger.Info().Msg("Filesystem unmounted successfully")
			return
		}
		b.logger.Warn().Err(err).Msg("Unmount failed, forcing unmount")
	case <-ctx.Done():
		b.logger.Warn().Err(ctx.Err()).Msg("Unmount timed out, forcing unmount")
	}
	// ctx may already be done; the unmounter bounds its own attempts.
	b.forceUnmount(context.WithoutCancel(ctx))
}

// mountOptions builds the go-fuse options for this mount.
func (b *Backend) mountOptions() *fs.Options {
	mountOpt := fuse.MountOptions{
		FsName:               "decypharr",
		Debug:                false,
		Name:                 "decypharr",
		DisableXAttrs:        true,
		IgnoreSecurityLabels: true,
		MaxWrite:             maxWrite,
		// The kernel defaults MaxBackground to 12, which caps in-flight
		// readahead far below the VFS readahead window.
		MaxBackground: b.config.FuseMaxBackground,
		MaxReadAhead:  b.config.FuseMaxReadAhead,
		AllowOther:    true,
		// Route handler panics through our logger; go-fuse fails the single
		// request with EIO instead of the panic unwinding into its serve loop.
		PanicHandler: func(p any) fuse.Status {
			b.logger.Error().Any("panic", p).Bytes("stack", debug.Stack()).Msg("FUSE handler panic")
			return fuse.EIO
		},
	}

	var opt []string

	opt = append(opt, "default_permissions")

	if runtime.GOOS == "darwin" {
		opt = append(opt, "volname=decypharr")
		opt = append(opt, "noapplexattr")
		opt = append(opt, "noappledouble")
	}

	mountOpt.Options = opt

	// Configure FUSE options
	// Use short entry timeout (1s) to ensure new files appear quickly
	entryTimeout := EntryTimeout
	attrTimeout := AttrTimeout
	opts := &fs.Options{
		AttrTimeout:  &attrTimeout,
		EntryTimeout: &entryTimeout,
		MountOptions: mountOpt,
		UID:          b.config.UID,
		GID:          b.config.GID,
	}
	return opts
}

// Unmount unmounts the filesystem.
func (b *Backend) Unmount(ctx context.Context) error {
	b.logger.Info().Msg("Unmounting hanwen backend")
	if b.unmountFunc != nil {
		// unmountFunc already closes the VFS manager as part of its teardown;
		// don't close it a second time here.
		b.unmountFunc(ctx)
		return nil
	}
	// Mount never completed: force-unmount and close the VFS ourselves.
	b.forceUnmount(ctx)
	if b.vfs != nil {
		if err := b.vfs.Close(); err != nil {
			b.logger.Warn().Err(err).Msg("Failed to close VFS")
		}
	}
	return nil
}

// WaitReady waits for the mount to be ready.
func (b *Backend) WaitReady(_ context.Context) error {
	if b.server == nil {
		return fmt.Errorf("server not initialized")
	}
	return b.server.WaitMount()
}

// IsReady returns true if the mount is ready.
func (b *Backend) IsReady() bool {
	return b.ready.Load()
}

// Type returns the backend type.
func (b *Backend) Type() backend.Type {
	return backend.Hanwen
}

func (b *Backend) Refresh(dir string) {
	// Refresh the root dir first
	if b.root != nil {
		b.root.Refresh()
		if dir != "" {
			b.root.RefreshChild(dir)
		}
	}
}

// forceUnmount detaches the mount point without the FUSE server, for a
// stale mount or one whose server did not come up.
func (b *Backend) forceUnmount(ctx context.Context) {
	if err := b.unmounter.Unmount(ctx, b.config.MountPath); err != nil {
		b.logger.Debug().Err(err).Msg("Force unmount did not detach the mount point")
	}
}
