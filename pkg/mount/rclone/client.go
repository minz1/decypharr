package rclone

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/fsutil"
	"github.com/sirrobot01/decypharr/internal/utils"
)

// mountOnce makes one mount attempt. forget first asks rclone to drop its
// registration of a gone mount, within rcdRCUnmountBudget so a hung rclone
// leaves the rest of the attempt its time; cleanup then detaches a stale mount
// point. Both are best effort: with nothing to drop or detach they fail, and
// that must not block the mount.
func (m *Manager) mountOnce(ctx context.Context, cleanup, forget bool) error {
	cfg := m.mount
	if forget {
		forgetCtx, cancel := context.WithTimeout(ctx, rcdRCUnmountBudget)
		err := m.client.Unmount(forgetCtx, cfg.MountPath)
		cancel()
		if err != nil {
			m.logger.Debug().Err(err).Msg("rclone had no mount to forget")
		}
	}
	if cleanup {
		if err := m.forceUnmount(ctx); err != nil {
			m.logger.Debug().Err(err).Msg("No stale mount to clean up")
		}
	}
	if runtime.GOOS != "windows" {
		_ = fsutil.MkdirShared(cfg.MountPath, m.mountDirMode)
	}
	if err := m.createConfig(ctx); err != nil {
		return err
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
		if detachErr := m.forceUnmount(ctx); detachErr != nil {
			m.logger.Debug().Err(detachErr).Msg("Nothing to detach after the failed mount")
		}
		return fmt.Errorf("failed to mount %s via RC: %w", cfg.MountPath, err)
	}
	return nil
}

// waitMountWork closes the mount work and waits for it on half the unmount budget, so a mount attempt still
// unwinding cannot land after the unmount. A parent cancel starts no shutdown of ops, so Close does; after Stop it is
// already done. On a timeout the unmount runs anyway: a mount left attached outlives the process.
func (m *Manager) waitMountWork() {
	if m.ops == nil {
		return
	}
	m.ops.Close()
	timer := time.NewTimer(rcdMountWorkWait)
	defer timer.Stop()
	select {
	case <-m.ops.Done():
	case <-timer.C:
		m.logger.Warn().Msg("Mount work still running at unmount")
	}
}

// unmount takes the mount down before rcd is signalled; it is the last move of
// the mount's state, made after the mount work has stopped (waitMountWork). A
// stop from Ready asks rcd first, within rcdRCUnmountBudget, and falls back to
// the unmount helpers on what is left of ctx. From any other state no live rcd
// holds a mount, as mounts are made only once rcd is Ready, so the old mount is
// a dead endpoint and the helpers run at once. A mount that was ever attempted
// is unmounted whatever its state: rcd may hold it even after a failed attempt.
func (m *Manager) unmount(ctx context.Context, from rcdState) {
	attempted := false
	if !m.moveMountWith(func(cur mountView) (mountState, func(*mountView)) {
		attempted = cur.attempted
		if !attempted {
			return mountUnmounted, nil
		}
		return mountUnmounting, nil
	}) {
		// The state is wrong, the mount point may not be: unmount anyway.
		m.logger.Warn().Str("mount", m.view.Load().state.String()).Msg("The mount's state refused the unmount")
	}
	if !attempted {
		m.logger.Info().Msg("No mount was attempted, nothing to unmount")
		return
	}

	m.waitMountWork()
	m.logger.Info().Msg("Unmounting")
	var err error
	force := from != rcdReady
	if !force {
		// The RC try takes at most half of what the mount-work wait left, so the forced fallback keeps a live share.
		rcBudget := rcdRCUnmountBudget
		if deadline, ok := ctx.Deadline(); ok {
			rcBudget = min(rcBudget, time.Until(deadline)/rcdRCShareDiv)
		}
		rcCtx, cancel := context.WithTimeout(ctx, rcBudget)
		err = m.client.Unmount(rcCtx, m.mount.MountPath)
		cancel()
		if err != nil {
			m.logger.Warn().Err(err).Msg("RC unmount failed, trying force unmount")
			force = true
		}
	}
	if force {
		// The forced unmount is the last step, so its outcome is the one kept.
		err = m.forceUnmount(ctx)
		if err != nil {
			m.logger.Error().Err(err).Msg("Force unmount failed")
		}
	}
	// A refused first move left the state behind: step it into Unmounting now,
	// or Unmounted is refused and a stopped manager reads as mounted.
	if s := m.view.Load().state; s != mountUnmounting && s != mountUnmounted {
		m.moveMount(mountUnmounting, nil)
	}
	if !m.moveMount(mountUnmounted, withErr(err)) {
		m.logger.Warn().Str("mount", m.view.Load().state.String()).Msg("The mount's state refused Unmounted")
	}
	m.logger.Info().Msg("Unmount completed")
}

// createConfig creates an rclone config entry for the provider.
func (m *Manager) createConfig(ctx context.Context) error {
	args := map[string]any{
		"name": ConfigName,
		"type": "webdav",
		"parameters": map[string]any{
			"url":             m.webdavURL,
			"vendor":          "other",
			"pacer_min_sleep": "0",
		},
	}
	if err := m.client.CreateConfig(ctx, args); err != nil {
		return fmt.Errorf("failed to create rclone config: %w", err)
	}
	return nil
}

// forceUnmount detaches the mount point without rclone's help, for a mount
// whose rclone is gone or no longer answers.
func (m *Manager) forceUnmount(ctx context.Context) error {
	if err := m.detach(ctx, m.mount.MountPath); err != nil {
		return err
	}
	m.logger.Info().Str("path", m.mount.MountPath).Msg("Force-unmounted the mount point")
	return nil
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
