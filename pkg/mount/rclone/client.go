package rclone

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/retry"
	"github.com/sirrobot01/decypharr/internal/utils"
)

// forceUnmountTimeout bounds the whole chain of umount fallbacks.
const forceUnmountTimeout = 10 * time.Second

// mountWithRetry attempts to mount with retry logic using avast/retry-go.
func (m *Manager) mountWithRetry(ctx context.Context, maxRetries int) error {
	return retry.Do(
		func() error {
			return m.performMount(ctx)
		},
		retry.Attempts(uint(maxRetries)+1),
		retry.Delay(config.DefaultRetryDelay),
		retry.DelayType(retry.FixedDelay),
		retry.If(func(_ error) bool {
			return true // Always retry on error
		}),
	)
}

// performMount performs a single mount attempt.
func (m *Manager) performMount(ctx context.Context) error {
	cfg := m.mount

	// Create mount directory if not on windows
	if runtime.GOOS != "windows" {
		_ = os.MkdirAll(cfg.MountPath, 0o755) //nolint:gosec // G301: mountpoint is shared (allow_other)
	}

	// Check if already mounted
	mountInfo := m.getMountInfo()

	if mountInfo != nil && mountInfo.Mounted {
		m.logger.Info().Msg("Already mounted")
		return nil
	}

	// Clean up any stale mount first. Best effort: when nothing is mounted
	// every umount fails, and that must not block the remount.
	if mountInfo != nil && !mountInfo.Mounted {
		if err := m.forceUnmount(ctx); err != nil {
			m.logger.Debug().Err(err).Msg("No stale mount to clean up")
		}
	}

	// Create rclone config for this provider
	if err := m.createConfig(); err != nil {
		return fmt.Errorf("failed to create rclone config: %w", err)
	}

	mountArgs := map[string]any{
		"fs":         FSName,
		"mountPoint": cfg.MountPath,
		"vfsOpt":     vfsOptions(cfg.Rclone),
		"mountOpt":   mountOptions(cfg.Rclone),
	}
	if cfg.Rclone.BufferSize != "" {
		// Only add _config if there are options to set
		mountArgs["_config"] = map[string]any{"BufferSize": cfg.Rclone.BufferSize}
	}

	if err := m.client.Mount(ctx, mountArgs); err != nil {
		_ = m.forceUnmount(ctx)
		return fmt.Errorf("failed to mount %s via RC: %w", cfg.MountPath, err)
	}

	// Store mount info
	mntInfo := &MountInfo{
		LocalPath:  cfg.MountPath,
		WebDAVURL:  m.webdavURL,
		Mounted:    true,
		MountedAt:  time.Now().Format(time.RFC3339),
		ConfigName: ConfigName,
	}

	m.info.Store(mntInfo)

	return nil
}

// unmount is the internal unmount function.
func (m *Manager) unmount(ctx context.Context) {
	mountInfo := m.getMountInfo()

	if mountInfo == nil || !mountInfo.Mounted {
		m.logger.Info().Msg("Mount not found or already unmounted")
		return
	}

	m.logger.Info().Msg("Unmounting")

	// Try RC unmount first

	err := m.client.Unmount(ctx, mountInfo.LocalPath)

	// If RC unmount fails or server is not ready, try force unmount
	if err != nil {
		m.logger.Warn().Err(err).Msg("RC unmount failed, trying force unmount")
		if forceUnmountErr := m.forceUnmount(ctx); forceUnmountErr != nil {
			m.logger.Error().Err(forceUnmountErr).Msg("Force unmount failed")
			// Don't return error here, update the state anyway
		}
	}

	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	m.markUnmounted(errMsg)
	m.logger.Info().Msg("Unmount completed")
}

// markUnmounted publishes a copy of the mount info flagged unmounted. The
// stored MountInfo is shared with concurrent readers (Stats, IsMounted), so it
// is replaced, never mutated in place.
func (m *Manager) markUnmounted(errMsg string) {
	current := m.getMountInfo()
	if current == nil {
		return
	}
	next := *current
	next.Mounted = false
	next.Error = errMsg
	m.info.Store(&next)
}

// createConfig creates an rclone config entry for the provider.
func (m *Manager) createConfig() error {
	args := map[string]any{
		"name": ConfigName,
		"type": "webdav",
		"parameters": map[string]any{
			"url":             m.webdavURL,
			"vendor":          "other",
			"pacer_min_sleep": "0",
		},
	}
	if err := m.client.CreateConfig(context.Background(), args); err != nil {
		return fmt.Errorf("failed to create rclone config: %w", err)
	}
	return nil
}

// forceUnmount attempts to force unmount a path using system commands.
func (m *Manager) forceUnmount(ctx context.Context) error {
	mountPath := m.mount.MountPath
	methods := [][]string{
		{"umount", mountPath},
		{"umount", "-l", mountPath}, // lazy unmount
		{"fusermount", "-uz", mountPath},
		{"fusermount3", "-uz", mountPath},
	}

	ctx, cancel := context.WithTimeout(ctx, forceUnmountTimeout)
	defer cancel()

	for _, method := range methods {
		if err := m.tryUnmountCommand(ctx, method...); err == nil {
			m.logger.Info().
				Strs("command", method).
				Msg("Successfully unmounted using system command")
			return nil
		}
	}

	return fmt.Errorf("all force unmount attempts failed for %s", mountPath)
}

// tryUnmountCommand tries to run an unmount command.
func (m *Manager) tryUnmountCommand(ctx context.Context, args ...string) error {
	if len(args) == 0 {
		return fmt.Errorf("no command provided")
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // G204: fixed umount commands, no shell
	return cmd.Run()
}

// mountOptions builds rclone's mountOpt from the config.
func mountOptions(rc config.Rclone) map[string]any {
	opt := map[string]any{
		"AllowNonEmpty": true,
		"AllowOther":    true,
		"DebugFUSE":     false,
		"DeviceName":    "decypharr",
		"VolumeName":    "decypharr",
	}
	if rc.AsyncRead != nil {
		opt["AsyncRead"] = *rc.AsyncRead
	}
	if rc.UseMmap {
		opt["UseMmap"] = true
	}
	if rc.Transfers != 0 {
		opt["Transfers"] = rc.Transfers
	}
	if rc.AttrTimeout != "" {
		if attrTimeout, err := utils.ParseDuration(rc.AttrTimeout); err == nil {
			opt["AttrTimeout"] = attrTimeout.String()
		}
	}
	return opt
}

// vfsOptions builds rclone's vfsOpt from the config. Cache tuning applies
// only when the VFS cache is on.
func vfsOptions(rc config.Rclone) map[string]any {
	opt := map[string]any{
		"CacheMode":    rc.VfsCacheMode,
		"DirCacheTime": rc.DirCacheTime,
		"PollInterval": 0, // Poll interval not supported for webdav, set to 0
	}
	if rc.VfsCacheMode != "off" {
		setIfNonEmpty(opt, map[string]string{
			"CacheMaxAge":        rc.VfsCacheMaxAge,
			"DiskSpaceTotalSize": rc.VfsDiskSpaceTotal,
			"ChunkSizeLimit":     rc.VfsReadChunkSizeLimit,
			"CacheMaxSize":       rc.VfsCacheMaxSize,
			"CachePollInterval":  rc.VfsCachePollInterval,
			"ChunkSize":          rc.VfsReadChunkSize,
			"ReadAhead":          rc.VfsReadAhead,
			"CacheMinFreeSpace":  rc.VfsCacheMinFreeSpace,
		})
		setIfTrue(opt, map[string]bool{
			"FastFingerprint": rc.VfsFastFingerprint,
			"NoChecksum":      rc.NoChecksum,
			"NoModTime":       rc.NoModTime,
		})
		if rc.VfsReadChunkStreams != 0 {
			opt["ChunkStreams"] = rc.VfsReadChunkStreams
		}
	}
	if rc.UID != 0 {
		opt["UID"] = rc.UID
	}
	if rc.GID != 0 {
		opt["GID"] = rc.GID
	}
	if rc.Umask != "" {
		if umask, err := strconv.ParseUint(rc.Umask, 8, 32); err == nil {
			opt["Umask"] = uint32(umask)
		}
	}
	return opt
}

func setIfNonEmpty(opt map[string]any, values map[string]string) {
	for k, v := range values {
		if v != "" {
			opt[k] = v
		}
	}
}

func setIfTrue(opt map[string]any, values map[string]bool) {
	for k, v := range values {
		if v {
			opt[k] = true
		}
	}
}
