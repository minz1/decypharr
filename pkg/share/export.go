package share

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/facetfs"
	"github.com/sirrobot01/facetfs/facetcache"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/manager"
)

// overshootInterval is how often the cache's disk accounting is checked
// against its budget.
const overshootInterval = 5 * time.Minute

// Export is the catalog exposed to the protocol servers. NFS and SMB share
// one: they serve the same tree, and two caches over one directory would
// delete each other's files.
type Export struct {
	fsys  facetfs.FileSystem
	cache *facetcache.Cache
	log   zerolog.Logger

	closeOnce sync.Once
	closeErr  error
}

// NewExport builds the catalog filesystem and, unless disabled, the on-disk
// read cache in front of it. g owns the export: its context bounds the
// streaming sessions the cache opens, and the cache closes when g stops.
// Create g before the groups of the servers that use the export, so they
// stop first.
func NewExport(g *task.Group, mgr *manager.Manager, cfg config.ShareCache, log zerolog.Logger) (*Export, error) {
	ctx := g.Context()
	streams := &streamer{ctx: ctx, mgr: mgr}
	catalog := newFilesystem(mgr, streams.open)

	if !cfg.IsEnabled() {
		// The default. Say it at debug only; an Info line on every start would
		// report the absence of an opt-in feature.
		log.Debug().Msg("Share cache disabled; reads stream straight from the debrid")
		return &Export{fsys: catalog, log: log}, nil
	}

	cache := &facetcache.Cache{
		Backend:   catalog,
		Dir:       cfg.Dir,
		MaxBytes:  cfg.MaxSizeBytes(),
		MaxAge:    cfg.MaxAgeDuration(),
		ChunkSize: cfg.ChunkSizeBytes(),
		ReadAhead: cfg.ReadAheadBytes(),
		// Cache faults are recoverable — a read that cannot be cached is
		// served from the backend instead — so they are not warnings. Paths
		// the cache refuses (a torrent name with a backslash, a name past the
		// filesystem's limit) log here on every open.
		Logger: func(err error) { log.Debug().Err(err).Msg("Share cache fault") },
	}
	fsys, err := cache.FileSystem()
	if err != nil {
		return nil, fmt.Errorf("start share cache in %s: %w", cfg.Dir, err)
	}

	stats := cache.Stats()
	log.Info().
		Str("dir", cfg.Dir).
		Str("max_size", humanBytes(stats.MaxBytes)).
		Str("cached", humanBytes(stats.CachedBytes)).
		Msg("Share cache started")

	e := &Export{fsys: fsys, cache: cache, log: log}
	if err = g.Go("budget", func(ctx context.Context) error {
		e.watchBudget(ctx)
		return nil
	}); err != nil {
		return nil, errors.Join(err, e.Close())
	}
	if err = g.Go("close", func(ctx context.Context) error {
		<-ctx.Done()
		return e.Close()
	}); err != nil {
		return nil, errors.Join(err, e.Close())
	}
	return e, nil
}

// FileSystem returns the tree the protocol servers export.
func (e *Export) FileSystem() facetfs.FileSystem { return e.fsys }

// Close stops the cache once; later calls return the first result. The
// export's group calls it when it stops, after the protocol servers.
func (e *Export) Close() error {
	e.closeOnce.Do(func() {
		if e.cache != nil {
			e.closeErr = e.cache.Close()
		}
	})
	return e.closeErr
}

// watchBudget reports a cache that stays over its budget. Whole-file eviction
// cannot touch a file a client holds open, so the cache reclaims inside open
// files by punching holes — which ZFS and some overlay setups refuse. There
// the budget is advisory and the disk keeps filling, so say so rather than
// letting it grow quietly.
func (e *Export) watchBudget(ctx context.Context) {
	t := time.NewTicker(overshootInterval)
	defer t.Stop()
	over := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s := e.cache.Stats()
		if s.Overshoot <= 0 {
			over = false
			continue
		}
		if over {
			// Two passes over budget: reclamation is not keeping up.
			e.log.Warn().
				Str("cached", humanBytes(s.CachedBytes)).
				Str("max_size", humanBytes(s.MaxBytes)).
				Str("over", humanBytes(s.Overshoot)).
				Msg("Share cache is over its budget; the cache directory may be on a filesystem that cannot punch holes (ZFS, btrfs), so lower share_cache.max_size or move it")
		}
		over = true
	}
}

func humanBytes(n int64) string {
	const unit = 1 << 10
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}
