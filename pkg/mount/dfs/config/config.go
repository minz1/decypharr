package config

import (
	"fmt"
	"io/fs"
	"math"
	"os"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
)

// Streaming-optimized defaults.
const (
	defaultDaemonTimeout     = 10 * time.Second
	defaultCacheExpiry       = 24 * time.Hour
	defaultCleanupInterval   = 5 * time.Minute
	defaultChunkSize         = 4 << 20  // matches the beta baseline
	defaultReadAheadSize     = 16 << 20 // 4 chunks ahead
	defaultFuseMaxBackground = 64
	defaultFuseMaxReadAhead  = 1 << 20
	defaultRetries           = 3
	defaultOwnerID           = 1000
	defaultUmask             = 0o022
	readAheadShareDivisor    = 2 // a stream's disk share: half read-ahead, half history
)

// FuseConfig holds the simplified configuration for the FUSE filesystem.
type FuseConfig struct {
	MountPath string
	// MountDirMode creates a missing mount point and its parents (the
	// shared directory mode).
	MountDirMode fs.FileMode
	CacheDir     string
	Client       string
	DisableCache bool

	// Cache
	CacheDiskSize        int64 // in bytes
	CacheCleanupInterval time.Duration

	// BufferMemory is the RAM budget (bytes) for the DFS streaming-buffer pool,
	// shared across all open files. 0 disables the cap.
	BufferMemory int64

	CacheExpiry time.Duration

	// Performance settings
	ChunkSize     int64
	ReadAheadSize int64
	DaemonTimeout time.Duration

	// FUSE kernel tuning (hanwen backend). FuseMaxBackground raises the
	// kernel's in-flight background request cap (default 12) so readahead can
	// actually fill the pipe; FuseMaxReadAhead is the readahead window
	// advertised at init.
	FuseMaxBackground int
	FuseMaxReadAhead  int

	// DropBehindMargin, when > 0, makes the read path release the disk file's
	// page cache for data more than this many bytes behind the current read
	// offset (keeping the trailing margin resident so readahead/short
	// seek-backs are unaffected, and keeping the bytes on disk so a longer
	// seek-back re-reads locally rather than re-downloading). 0 disables it —
	// the default, since the page cache it trims is reclaimable and only worth
	// dropping under a tight memory cap.
	DropBehindMargin int64

	Retries int

	// File system settings
	UID   uint32
	GID   uint32
	Umask uint32
}

// DefaultFuseConfig returns a streaming-optimized default configuration.
func DefaultFuseConfig() *FuseConfig {
	return &FuseConfig{
		MountDirMode: (&config.Config{}).SharedDirModeValue(),
		// Performance defaults optimized for streaming
		DaemonTimeout:        defaultDaemonTimeout,
		CacheExpiry:          defaultCacheExpiry,
		CacheCleanupInterval: defaultCleanupInterval,
		ChunkSize:            defaultChunkSize,
		ReadAheadSize:        defaultReadAheadSize,
		FuseMaxBackground:    defaultFuseMaxBackground,
		FuseMaxReadAhead:     defaultFuseMaxReadAhead,
		Client:               "DFS",

		Retries: defaultRetries,

		// File system defaults
		UID:   defaultOwnerID,
		GID:   defaultOwnerID,
		Umask: defaultUmask,
	}
}

// Parse converts the DFS section of the main config into a FuseConfig. Invalid
// values keep their defaults and are reported on stderr.
func Parse(cfg config.DFS, mountPath string, retries int) *FuseConfig {
	fuseConfig := DefaultFuseConfig()

	fuseConfig.CacheDir = cfg.CacheDir
	fuseConfig.MountPath = mountPath
	fuseConfig.DisableCache = cfg.DisableCache
	fuseConfig.BufferMemory = cfg.BufferMemoryBytes()

	if cfg.DaemonTimeout != "" {
		if timeout, err := utils.ParseDuration(cfg.DaemonTimeout); err == nil {
			fuseConfig.DaemonTimeout = timeout
		}
	}
	// The DFS mount uses a single shared on-disk cache (one CacheDir, one
	// vfs.Cache), so the configured size is the cache budget verbatim. Do not
	// divide by the number of debrid providers. An invalid value should not
	// happen (loadConfig validates sizes first); CacheDiskSize then stays 0 so
	// IsOverBudget is never true.
	parseSizeField("disk_cache_size", cfg.DiskCacheSize, "cache enforcement DISABLED", &fuseConfig.CacheDiskSize)
	// The cleanup loop feeds this to time.NewTicker, which panics on a
	// non-positive interval and would take the process down.
	parseDurationField("cache_cleanup_interval", cfg.CacheCleanupInterval, true, &fuseConfig.CacheCleanupInterval)
	parseSizeField("chunk_size", cfg.ChunkSize, "using default", &fuseConfig.ChunkSize)
	parseDurationField("cache_expiry", cfg.CacheExpiry, false, &fuseConfig.CacheExpiry)
	parseSizeField("read_ahead_size", cfg.ReadAheadSize, "using default", &fuseConfig.ReadAheadSize)
	parseSizeField("drop_behind_margin", cfg.DropBehindMargin, "using default", &fuseConfig.DropBehindMargin)
	if cfg.FuseMaxBackground > 0 {
		fuseConfig.FuseMaxBackground = cfg.FuseMaxBackground
	}
	if cfg.FuseMaxReadAhead != "" {
		if size, err := config.ParseSize(cfg.FuseMaxReadAhead); err == nil && size > 0 && size <= math.MaxInt32 {
			fuseConfig.FuseMaxReadAhead = int(size)
		}
	}
	fuseConfig.UID = cfg.UID
	fuseConfig.GID = cfg.GID

	if cfg.Umask != "" {
		if umask, err := parseUmask(cfg.Umask); err == nil {
			fuseConfig.Umask = umask
		}
	}

	fuseConfig.Retries = retries

	fuseConfig.ReadAheadSize = reconcileReadAhead(
		fuseConfig.ReadAheadSize,
		fuseConfig.ChunkSize,
		fuseConfig.CacheDiskSize,
	)

	return fuseConfig
}

// parseSizeField stores a size setting in dst, or reports it and leaves dst
// untouched. Empty means unset.
func parseSizeField(field, value, fallback string, dst *int64) {
	if value == "" {
		return
	}
	size, err := config.ParseSize(value)
	if err != nil {
		reportInvalid(field, value, err, fallback)
		return
	}
	*dst = size
}

// parseDurationField stores a duration setting in dst, or reports it and
// leaves dst untouched. Empty means unset.
func parseDurationField(field, value string, positive bool, dst *time.Duration) {
	if value == "" {
		return
	}
	d, err := utils.ParseDuration(value)
	if err == nil && positive && d <= 0 {
		err = fmt.Errorf("interval must be positive, got %s", d)
	}
	if err != nil {
		reportInvalid(field, value, err, "using default")
		return
	}
	*dst = d
}

func reportInvalid(field, value string, err error, fallback string) {
	_, _ = fmt.Fprintf(os.Stderr, "[DFS] ERROR: invalid mount.dfs.%s %q: %v — %s\n", field, value, err, fallback)
}

// StreamDiskShare is how many concurrent streams the disk cache is budgeted
// for. One stream's share covers its read-ahead plus the history the pool
// keeps behind its read head; vfs.NewCache derives its back-window from it.
const StreamDiskShare = 4

// reconcileReadAhead clamps read-ahead against the disk budget it writes into.
// The shipped defaults contradict each other — 128MB ahead of a 500MB cache
// means two streams exceed the limit and the pool punches holes continuously —
// so any pair of values is made to settle rather than thrash.
func reconcileReadAhead(readAhead, chunkSize, diskLimit int64) int64 {
	if readAhead <= 0 || diskLimit <= 0 {
		return readAhead
	}
	maxAhead := diskLimit / StreamDiskShare / readAheadShareDivisor
	if readAhead <= maxAhead {
		return readAhead
	}
	return max(maxAhead, chunkSize) // must reach at least one chunk past the reader
}

// parseUmask parses umask strings like "0022".
func parseUmask(umaskStr string) (uint32, error) {
	var umask uint32
	if _, err := fmt.Sscanf(umaskStr, "%o", &umask); err != nil {
		return 0, fmt.Errorf("invalid umask format: %s", umaskStr)
	}
	return umask, nil
}
