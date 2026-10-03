package manager

import (
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/puzpuzpuz/xsync/v4"
	"golang.org/x/sync/singleflight"
)

const (
	torrentEntryCachePrefix = "torrent::"
)

func (m *Manager) initEntryCache() {
	m.entry = NewEntryCache(m)
}

type EntryCacheItem struct {
	current  *FileInfo
	children []FileInfo
	// generation is the cache generation the item was built in. An item from
	// an earlier generation predates an InvalidateAll and is never served.
	generation uint64
}

// entryLoader builds the listing for one cache key.
type entryLoader func(name string) (*FileInfo, []FileInfo)

type EntryCache struct {
	// timeSensitive reports views that must never be cached.
	timeSensitive func(name string) bool
	loadTorrent   entryLoader
	loadGroup     entryLoader

	entries    *xsync.Map[string, EntryCacheItem]
	refreshing singleflight.Group
	generation atomic.Uint64
}

func NewEntryCache(manager *Manager) *EntryCache {
	return newEntryCache(
		func(name string) bool { return manager.virtualFoldersSnapshot().IsTimeSensitive(name) },
		manager.getTorrentChildren,
		manager.getEntryChildren,
	)
}

func newEntryCache(timeSensitive func(string) bool, loadTorrent, loadGroup entryLoader) *EntryCache {
	return &EntryCache{
		timeSensitive: timeSensitive,
		loadTorrent:   loadTorrent,
		loadGroup:     loadGroup,
		entries:       xsync.NewMap[string, EntryCacheItem](),
	}
}

func (e *EntryCache) Get(name string) (*FileInfo, []FileInfo) {
	// Relative-time views change as the clock advances even when library
	// metadata does not, so never retain their children in the entry cache.
	if !strings.HasPrefix(name, torrentEntryCachePrefix) && e.timeSensitive(name) {
		return e.loadGroup(name)
	}
	generation := e.generation.Load()
	item, ok := e.entries.Load(name)
	if !ok || item.generation != generation {
		item = e.refreshEntry(name, generation)
	}
	return item.current, item.children
}

// refreshEntry loads name once per generation: a Get that follows an
// InvalidateAll never joins a load that started before it.
func (e *EntryCache) refreshEntry(name string, generation uint64) EntryCacheItem {
	key := strconv.FormatUint(generation, 10) + ":" + name
	result, _, _ := e.refreshing.Do(key, func() (any, error) {
		return e.load(name, generation), nil
	})
	item, _ := result.(EntryCacheItem) // always an EntryCacheItem; zero value on the impossible miss
	return item
}

func (e *EntryCache) load(name string, generation uint64) EntryCacheItem {
	var item EntryCacheItem
	if torrentName, ok := strings.CutPrefix(name, torrentEntryCachePrefix); ok {
		item.current, item.children = e.loadTorrent(torrentName)
	} else {
		// This is a built-in, provider, or virtual folder.
		item.current, item.children = e.loadGroup(name)
	}
	item.generation = generation
	// Keep whichever item is newer. A load that outlived an InvalidateAll may
	// still land here, but Get ignores it for its stale generation.
	e.entries.Compute(name, func(old EntryCacheItem, loaded bool) (EntryCacheItem, xsync.ComputeOp) {
		if loaded && old.generation > generation {
			return old, xsync.CancelOp
		}
		return item, xsync.UpdateOp
	})
	return item
}

// InvalidateAll clears cached entries. Reads rebuild them on demand.
func (e *EntryCache) InvalidateAll() {
	e.generation.Add(1)
	// Clear every group and torrent entry. This is deliberately independent of
	// the current config so renamed/removed virtual folders cannot survive in the
	// cache after a live update.
	e.entries.Clear()
}
