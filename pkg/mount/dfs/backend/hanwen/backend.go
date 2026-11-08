//go:build linux || (darwin && amd64)

package hanwen

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/fsutil"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/task"
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
	config    *config.FuseConfig
	logger    zerolog.Logger
	root      *Dir
	unmounter mountDetacher
}

// mountDetacher force-detaches a mount point (unmount.Unmounter).
type mountDetacher interface {
	Unmount(ctx context.Context, mountPath string) error
}

// fuseServer is the part of *fuse.Server the handle uses.
type fuseServer interface {
	Unmount() error
}

// mounted is the live mount: Done closes when go-fuse forgets the root, which
// it does after the serve loops have exited.
type mounted struct {
	server   fuseServer
	done     chan struct{}
	once     sync.Once
	path     string
	detacher mountDetacher
}

func newMounted(server fuseServer, path string, detacher mountDetacher) *mounted {
	return &mounted{server: server, done: make(chan struct{}), path: path, detacher: detacher}
}

// end closes Done once, however often go-fuse calls the root's hook.
func (m *mounted) end() { m.once.Do(func() { close(m.done) }) }

// Done is closed once when the serve loop has ended.
func (m *mounted) Done() <-chan struct{} { return m.done }

// Unmount is the graceful unmount: it returns once the kernel has detached the
// mount and the serve loop has exited, or with the reason it could not.
func (m *mounted) Unmount() error { return m.server.Unmount() }

// Detach lazily detaches the mount point.
func (m *mounted) Detach(ctx context.Context) error { return m.detacher.Unmount(ctx, m.path) }

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
		unmounter: unmount.New(log),
	}, nil
}

// Mount mounts the filesystem using hanwen/go-fuse. It runs in the dfs owner
// task and returns once the kernel has answered the mount.
func (b *Backend) Mount(ctx context.Context, _ *task.Group) (backend.Mounted, error) {
	if b.root == nil {
		return nil, fmt.Errorf("root node is not initialized")
	}

	_ = fsutil.MkdirShared(b.config.MountPath, b.config.MountDirMode)
	// Try to unmount if already mounted
	b.forceUnmount(ctx)

	handle := newMounted(nil, b.config.MountPath, b.unmounter)
	b.root.onServeEnd = handle.end

	b.logger.Info().
		Str("mount_path", b.config.MountPath).
		Msg("Waiting for mount to be ready")
	server, err := fs.Mount(b.config.MountPath, b.root, b.mountOptions())
	if err != nil {
		return nil, fmt.Errorf("failed to create mount: %w", err)
	}
	handle.server = server
	return handle, nil
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
