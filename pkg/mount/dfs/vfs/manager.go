package vfs

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/config"
)

// Manager manages VFS lifecycle.
type Manager struct {
	manager *manager.Manager
	cache   *Cache
	logger  zerolog.Logger

	ctx    context.Context
	cancel context.CancelFunc
}

// getFileAttempts bounds GetFile's retries against a janitor tearing the
// file's cache item down.
const getFileAttempts = 8

// NewManager creates a new VFS manager.
func NewManager(ctx context.Context, mgr *manager.Manager, config *config.FuseConfig) (*Manager, error) {
	ctx, cancel := context.WithCancel(ctx)

	cache, err := NewCache(ctx, mgr, config)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create cache: %w", err)
	}

	m := &Manager{
		manager: mgr,
		cache:   cache,
		logger:  logger.New("vfs"),
		ctx:     ctx,
		cancel:  cancel,
	}

	return m, nil
}

func (m *Manager) GetManager() *manager.Manager {
	return m.manager
}

// GetFile returns a file handle for reading. When the cache is at capacity,
// it returns a DirectStreamFile that fetches directly from the network without
// writing to disk, so reads degrade gracefully rather than failing with EIO.
func (m *Manager) GetFile(info *manager.FileInfo) (File, error) {
	// Cache disabled or over eviction threshold: bypass disk caching entirely.
	// The existing CacheItem (if any) stays alive until its last handle is
	// released; this handle just skips it and streams directly.
	if m.cache.config.DisableCache || m.cache.IsOverBudget() {
		entry, err := m.manager.GetEntryByName(info.Parent(), info.Name())
		if err != nil {
			return nil, fmt.Errorf("cache full, direct stream unavailable: %w", err)
		}
		m.logger.Info().
			Str("entry", info.Parent()).
			Str("file", info.Name()).
			Bool("disabled", m.cache.config.DisableCache).
			Msg("cache over budget, serving direct")
		return newDirectStreamFile(m.manager, entry, info.Name(), info.Size(), m.cache.config.Retries), nil
	}

	// NewStreamingFile returns nil when the item was claimed for teardown by
	// the cache janitor between our GetItem and Open; GetItem waits such an
	// item out, so a retry gets a fresh one. The claim/delete pair is
	// near-instantaneous, so the loop is bounded in practice.
	for range getFileAttempts {
		item, err := m.cache.GetItem(info.Parent(), info.Name(), info.Size())
		if err != nil {
			return nil, fmt.Errorf("failed to get cache item: %w", err)
		}
		if sf := NewStreamingFile(item); sf != nil {
			return sf, nil
		}
	}
	return nil, fmt.Errorf("file %s: cache item kept being torn down; giving up", buildCacheKey(info.Parent(), info.Name()))
}

// Close shuts down the manager.
func (m *Manager) Close() error {
	m.cancel()

	// Close cache
	if m.cache != nil {
		m.cache.Close()
	}

	return nil
}

// GetStats returns manager statistics.
func (m *Manager) GetStats() map[string]any {
	stats := map[string]any{
		"type":         "dfs",
		"ready":        true,
		"enabled":      true,
		"total_files":  int32(0),
		"active_files": int32(0),
	}

	// Add cache stats
	if m.cache != nil {
		stats["total_files"] = m.cache.totalFiles.Load()
		stats["active_files"] = m.cache.activeFiles.Load()
		for k, v := range m.cache.GetStats() {
			stats["cache_"+k] = v
		}
	}

	return stats
}

func (m *Manager) CleanupCache() map[string]any {
	if m.cache == nil {
		return map[string]any{
			"cleanup_status": "unsupported",
			"cleanup_result": "cache is not initialized",
		}
	}
	return m.cache.RunCleanup()
}

func (m *Manager) PurgeCache() map[string]any {
	if m.cache == nil {
		return map[string]any{
			"purge_status": "unsupported",
			"purge_result": "cache is not initialized",
		}
	}
	return m.cache.PurgeCache()
}
