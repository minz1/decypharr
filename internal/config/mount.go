package config

import (
	"fmt"
	"time"
)

type Rclone struct {
	// Global mount folder where all providers will be mounted as subfolders
	Enabled   bool   `json:"enabled,omitempty"`
	MountPath string `json:"mount_path,omitempty"`
	Port      string `json:"port,omitempty"`
	// Cache settings
	CacheDir string `json:"cache_dir,omitempty"`
	// VFS settings
	VfsCacheMode          string `json:"vfs_cache_mode,omitempty"`            // off, minimal, writes, full
	VfsCacheMaxAge        string `json:"vfs_cache_max_age,omitempty"`         // Maximum age of objects in the cache (default 1h)
	VfsDiskSpaceTotal     string `json:"vfs_disk_space_total,omitempty"`      // Total disk space available for the cache (default off)
	VfsCacheMaxSize       string `json:"vfs_cache_max_size,omitempty"`        // Maximum size of the cache (default off)
	VfsCachePollInterval  string `json:"vfs_cache_poll_interval,omitempty"`   // How often to poll for changes (default 1m)
	VfsReadChunkSize      string `json:"vfs_read_chunk_size,omitempty"`       // Read chunk size (default 128M)
	VfsReadChunkSizeLimit string `json:"vfs_read_chunk_size_limit,omitempty"` // Max chunk size (default off)
	VfsReadAhead          string `json:"vfs_read_ahead,omitempty"`            // read ahead size
	BufferSize            string `json:"buffer_size,omitempty"`               // Buffer size for reading files (default 16M)
	BwLimit               string `json:"bw_limit,omitempty"`                  // Bandwidth limit (default off)

	VfsCacheMinFreeSpace string `json:"vfs_cache_min_free_space,omitempty"`
	VfsFastFingerprint   bool   `json:"vfs_fast_fingerprint,omitempty"`
	VfsReadChunkStreams  int    `json:"vfs_read_chunk_streams,omitempty"`
	AsyncRead            *bool  `json:"async_read,omitempty"` // Use async read for files
	Transfers            int    `json:"transfers,omitempty"`  // Number of transfers to use (default 4)
	UseMmap              bool   `json:"use_mmap,omitempty"`

	// File system settings
	UID   uint32 `json:"uid,omitempty"` // User ID for mounted files
	GID   uint32 `json:"gid,omitempty"` // Group ID for mounted files
	Umask string `json:"umask,omitempty"`

	// Timeout settings
	AttrTimeout  string `json:"attr_timeout,omitempty"`   // Attribute cache timeout (default 1s)
	DirCacheTime string `json:"dir_cache_time,omitempty"` // Directory cache time (default 5m)

	// Performance settings
	NoModTime  bool `json:"no_modtime,omitempty"`  // Don't read/write modification time
	NoChecksum bool `json:"no_checksum,omitempty"` // Don't checksum files on upload

	LogLevel string `json:"log_level,omitempty"`
}

func (r Rclone) IsZero() bool {
	return !r.Enabled && r.MountPath == "" && r.Port == "" && r.CacheDir == ""
}

type DFS struct {
	// Core settings
	CacheExpiry          string `json:"cache_expiry,omitempty"`           // 1h, 30m etc
	CacheDir             string `json:"cache_dir,omitempty"`              // /tmp/decypharr-cache
	DiskCacheSize        string `json:"disk_cache_size,omitempty"`        // 10GB, 50GB etc
	CacheCleanupInterval string `json:"cache_cleanup_interval,omitempty"` // 10m, 1h etc
	DisableCache         bool   `json:"disable_cache,omitempty"`          // stream everything direct, no disk writes

	// BufferMemory caps the total RAM the DFS streaming buffers hold across all
	// open files, e.g. "512MB". Reads are served from this window, so it is
	// what keeps playback off the disk; per-file windows are sized from
	// read_ahead_size and share this budget when several streams are open.
	// Empty = default (512MB); "0" disables the cap.
	BufferMemory string `json:"buffer_memory,omitempty"`

	// Performance settings
	ChunkSize     string `json:"chunk_size,omitempty"`      // Initial chunk size, e.g 10MB
	ReadAheadSize string `json:"read_ahead_size,omitempty"` // Read ahead size (deprecated, use MaxChunkSize)

	// DropBehindMargin, e.g "256MB", makes the read path drop the page cache for
	// streamed data more than this far behind the read head (bytes stay on disk).
	// Empty/0 = disabled. Linux only; only useful under a tight memory cap.
	DropBehindMargin string `json:"drop_behind_margin,omitempty"`

	DaemonTimeout string `json:"daemon_timeout,omitempty"` // Time after which the FUSE daemon will exit if idle

	// FuseMaxBackground caps how many background FUSE requests (readahead,
	// async reads) the kernel keeps in flight. Kernel default is 12, which
	// throttles streaming readahead; DFS defaults to 64. 0 = default.
	FuseMaxBackground int `json:"fuse_max_background,omitempty"`
	// FuseMaxReadAhead is the readahead window advertised to the kernel,
	// e.g. "1MB". Empty = DFS default (1MB).
	FuseMaxReadAhead string `json:"fuse_max_read_ahead,omitempty"`

	// File system settings
	UID   uint32 `json:"uid,omitempty"`   // User ID for mounted files
	GID   uint32 `json:"gid,omitempty"`   // Group ID for mounted files
	Umask string `json:"umask,omitempty"` // File permissions mask
}

// Validate checks that every non-empty size/duration string can be parsed.
// Called from loadConfig after env overrides so that a bad value fails loudly
// at startup rather than silently disabling cache enforcement.
func (d DFS) Validate() error {
	sizes := []struct {
		field, value string
	}{
		{"mount.dfs.disk_cache_size", d.DiskCacheSize},
		{"mount.dfs.chunk_size", d.ChunkSize},
		{"mount.dfs.read_ahead_size", d.ReadAheadSize},
		{"mount.dfs.drop_behind_margin", d.DropBehindMargin},
		{"mount.dfs.buffer_memory", d.BufferMemory},
	}
	for _, s := range sizes {
		if s.value == "" {
			continue
		}
		if _, err := ParseSize(s.value); err != nil {
			return fmt.Errorf("invalid %s %q: %w", s.field, s.value, err)
		}
	}
	durations := []struct {
		field, value string
	}{
		{"mount.dfs.cache_expiry", d.CacheExpiry},
		{"mount.dfs.cache_cleanup_interval", d.CacheCleanupInterval},
		{"mount.dfs.daemon_timeout", d.DaemonTimeout},
	}
	for _, dur := range durations {
		if dur.value == "" {
			continue
		}
		if _, err := time.ParseDuration(dur.value); err != nil {
			return fmt.Errorf("invalid %s %q: %w", dur.field, dur.value, err)
		}
	}
	return nil
}

// DiskCacheSizeBytes resolves the DFS on-disk cache budget in bytes. Empty or
// unparseable -> 0 (unlimited). This is the disk limit handed to the DFS buffer
// pool, which punches holes behind the read head once an open stream pushes the
// cache past it.
func (d DFS) DiskCacheSizeBytes() int64 {
	if d.DiskCacheSize == "" {
		return 0
	}
	n, err := ParseSize(d.DiskCacheSize)
	if err != nil {
		return 0
	}
	return n
}

// defaultBufferMemory is the streaming-buffer RAM cap used when unset.
const defaultBufferMemory = 512 * mib

// BufferMemoryBytes resolves the DFS streaming-buffer RAM cap. Empty -> 512MB
// default; "0" -> disabled (0).
func (d DFS) BufferMemoryBytes() int64 {
	return bufferMemoryBytes(d.BufferMemory)
}

// bufferMemoryBytes parses a buffer_memory setting, falling back to
// defaultBufferMemory when it is empty or invalid.
func bufferMemoryBytes(value string) int64 {
	if value == "" {
		return defaultBufferMemory
	}
	n, err := ParseSize(value)
	if err != nil {
		return defaultBufferMemory
	}
	return n
}

type ExternalRclone struct {
	RCUrl      string `json:"rc_url,omitempty"`
	RCUsername string `json:"rc_username,omitempty"`
	RCPassword string `json:"rc_password,omitempty"`
}

type Mount struct {
	Type      MountType `json:"type,omitempty"`
	MountPath string    `json:"mount_path,omitempty"`

	Rclone         Rclone         `json:"rclone"`
	DFS            DFS            `json:"dfs"`
	ExternalRclone ExternalRclone `json:"external_rclone"`
}

func (c *Config) applyMountEnvVars() {
	// DFS settings
	envString("MOUNT__DFS__CACHE_DIR", &c.Mount.DFS.CacheDir)
	envString("MOUNT__DFS__CHUNK_SIZE", &c.Mount.DFS.ChunkSize)
	envString("MOUNT__DFS__READ_AHEAD_SIZE", &c.Mount.DFS.ReadAheadSize)
	envString("MOUNT__DFS__CACHE_EXPIRY", &c.Mount.DFS.CacheExpiry)
	envString("MOUNT__DFS__DISK_CACHE_SIZE", &c.Mount.DFS.DiskCacheSize)
	envBool("MOUNT__DFS__DISABLE_CACHE", &c.Mount.DFS.DisableCache)
	envString("MOUNT__DFS__CACHE_CLEANUP_INTERVAL", &c.Mount.DFS.CacheCleanupInterval)
	envString("MOUNT__DFS__DAEMON_TIMEOUT", &c.Mount.DFS.DaemonTimeout)
	envInt("MOUNT__DFS__FUSE_MAX_BACKGROUND", &c.Mount.DFS.FuseMaxBackground)
	envString("MOUNT__DFS__FUSE_MAX_READ_AHEAD", &c.Mount.DFS.FuseMaxReadAhead)
	envUint32("MOUNT__DFS__UID", &c.Mount.DFS.UID)
	envUint32("MOUNT__DFS__GID", &c.Mount.DFS.GID)
	envString("MOUNT__DFS__UMASK", &c.Mount.DFS.Umask)
	// Rclone settings
	envString("RCLONE__RC_PORT", &c.Mount.Rclone.Port)
	envString("RCLONE__LOG_LEVEL", &c.Mount.Rclone.LogLevel)
	envString("RCLONE__VFS_CACHE_MODE", &c.Mount.Rclone.VfsCacheMode)
	envString("RCLONE__CACHE_DIR", &c.Mount.Rclone.CacheDir)
	envInt("RCLONE__TRANSFERS", &c.Mount.Rclone.Transfers)
}
