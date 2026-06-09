package vfs

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"github.com/sirrobot01/decypharr/pkg/mount/dfs/config"
)

// Manager manages VFS lifecycle
type Manager struct {
	manager *manager.Manager
	cache   *Cache
	logger  zerolog.Logger

	files *xsync.Map[string, *fileEntry]

	ctx    context.Context
	cancel context.CancelFunc

	totalFiles  atomic.Int32
	activeFiles atomic.Int32
}

// fileEntry tracks file metadata
type fileEntry struct {
	item     *CacheItem
	refCount atomic.Int32
	// deleted is set to true by ReleaseFile before the entry is removed from the
	// map. GetFile checks this after incrementing refCount so it can detect and
	// undo a concurrent deletion without holding a coarse lock.
	deleted atomic.Bool
}

// NewManager creates a new VFS manager
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
		files:   xsync.NewMap[string, *fileEntry](),
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
	key := buildFileKey(info.Parent(), info.Name())

	overBudget := m.cache.config.DisableCache || m.cache.IsOverBudget()

	// Fast path: already cached — reuse the existing item only when under budget.
	// When over budget, fall through so this handle goes to direct streaming;
	// the existing CacheItem stays alive until its last handle is released.
	if !overBudget {
		if entry, ok := m.files.Load(key); ok {
			entry.refCount.Add(1)
			if !entry.deleted.Load() {
				return NewStreamingFile(entry.item), nil
			}
			entry.refCount.Add(-1)
		}
	}

	// Cache disabled or over eviction threshold: bypass disk caching entirely.
	if overBudget {
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

	// Normal path: get or create a cache item.
	item, err := m.cache.GetItem(info.Parent(), info.Name(), info.Size())
	if err != nil {
		return nil, fmt.Errorf("failed to get cache item: %w", err)
	}

	entry := &fileEntry{item: item}
	entry.refCount.Store(1)

	actual, loaded := m.files.LoadOrStore(key, entry)
	if loaded {
		actual.refCount.Add(1)
		return NewStreamingFile(actual.item), nil
	}

	m.totalFiles.Add(1)
	m.activeFiles.Add(1)
	return NewStreamingFile(item), nil
}

// ReleaseFile decrements the reference count
func (m *Manager) ReleaseFile(info *manager.FileInfo) {
	key := buildFileKey(info.Parent(), info.Name())

	if entry, ok := m.files.Load(key); ok {
		if entry.refCount.Add(-1) <= 0 {
			// Mark deleted before removing from the map so that any concurrent
			// GetFile that already loaded this entry can detect the deletion and
			// undo its refCount increment rather than using a stale entry.
			entry.deleted.Store(true)
			m.files.Delete(key)
			m.activeFiles.Add(-1)
			// Downloaders are stopped in CacheItem.Release() when opens reaches 0.
		}
	}
}

// Close shuts down the manager
func (m *Manager) Close() error {
	m.cancel()

	// Close all files
	m.files.Range(func(key string, entry *fileEntry) bool {
		if entry.item != nil {
			entry.item.Close()
		}
		return true
	})
	m.files.Clear()

	// Close cache
	if m.cache != nil {
		m.cache.Close()
	}

	return nil
}

// GetStats returns manager statistics
func (m *Manager) GetStats() map[string]interface{} {
	stats := map[string]interface{}{
		"type":         "dfs",
		"ready":        true,
		"enabled":      true,
		"total_files":  m.totalFiles.Load(),
		"active_files": m.activeFiles.Load(),
	}

	// Add cache stats
	if m.cache != nil {
		for k, v := range m.cache.GetStats() {
			stats["cache_"+k] = v
		}
	}

	return stats
}

func buildFileKey(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}
