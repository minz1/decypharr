package dfs

import (
	"sync/atomic"
)

// Stats provides unified statistics across all DFS mounts
// This aggregates stats from all mounted filesystems into a single view
type Stats struct {
	// Disk cache statistics
	CacheDirSize  atomic.Int64 // Total bytes used across all mounts
	CacheDirLimit atomic.Int64 // Total cache limit across all mounts

	// File operations
	OpenedFiles atomic.Int64 // Set of opened files
	ActiveReads atomic.Int64 // Total active read operations

	// Configuration (same across all mounts)
	ChunkSize     int64
	ReadAheadSize int64
	BufferSize    int64
}

// MountStats represents statistics for a single mount
type MountStats struct {
	Name      string
	Type      string
	Mounted   bool
	MountPath string

	// Cache
	CacheDirSize  int64
	CacheDirLimit int64

	// Operations
	OpenedFiles int
	ActiveReads int64
}
