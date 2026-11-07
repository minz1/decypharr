package rclone

import (
	"context"
	"fmt"
	"time"
)

// healthCheckInterval is how often the mount's health is probed.
const healthCheckInterval = 30 * time.Second

// RecoverMount attempts to recover a failed mount.
func (m *Manager) RecoverMount(ctx context.Context) error {
	mountInfo := m.getMountInfo()

	if mountInfo == nil {
		return fmt.Errorf("no mount info available for recovery")
	}

	m.logger.Warn().Msg("Attempting to recover mount")

	// Drop rclone's registration of the dead mount (best effort), then mount
	// again. This used to call Start, which returns immediately once the RC
	// server is up — so "recovery" never remounted anything.
	if err := m.client.Unmount(ctx, mountInfo.LocalPath); err != nil {
		m.logger.Debug().Err(err).Msg("RC unmount before recovery failed")
	}
	m.markUnmounted("")
	if err := m.mountWithRetry(ctx, mountRetries); err != nil {
		return fmt.Errorf("failed to recover mount: %w", err)
	}

	m.logger.Info().Msg("Successfully recovered mount")
	return nil
}

// MonitorMounts continuously monitors mount health and attempts recovery.
func (m *Manager) MonitorMounts(ctx context.Context) {
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.logger.Debug().Msg("Mount monitoring stopped")
			return
		case <-ticker.C:
			m.performMountHealthCheck(ctx)
		}
	}
}

// performMountHealthCheck checks and attempts to recover unhealthy mounts.
// While rcd is not Ready the owner is already acting, so it checks nothing.
func (m *Manager) performMountHealthCheck(ctx context.Context) {
	if !m.IsReady() {
		return
	}
	if err := m.client.CheckMountHealth(ctx, FSName); err != nil {
		m.logger.Warn().Err(err).Msg("Mount health check failed, attempting recovery")

		if m.getMountInfo() == nil {
			return
		}
		// One recovery at a time: a remount with retries can outlast the
		// next health tick.
		if !m.recovering.CompareAndSwap(false, true) {
			return
		}
		m.markUnmounted("Health check failed")
		if goErr := m.ops.Go("recover", func(ctx context.Context) error {
			defer m.recovering.Store(false)
			if recoverMountErr := m.RecoverMount(ctx); recoverMountErr != nil {
				m.logger.Error().Err(recoverMountErr).Msg("Failed to recover mount")
			}
			return nil
		}); goErr != nil {
			m.recovering.Store(false)
			m.logger.Warn().Err(goErr).Msg("Recovery skipped: the manager is stopping")
		}
	}
}
