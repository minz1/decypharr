package nntp

import (
	nntpyenc "github.com/sirrobot01/decypharr/internal/nntp/yenc"
)

type YencMetadata = nntpyenc.Metadata

// Response represents an NNTP server response.
type Response struct {
	Code    int
	Message string
}

// StatResult represents the result of a STAT command for a single message ID.
type StatResult struct {
	MessageID string // The message ID that was checked
	Available bool   // Whether the article is available
	Error     error  // Error if any (nil means success or article found)
}

// BatchStatResult contains results for all message IDs in a batch.
type BatchStatResult struct {
	Results    []StatResult // Per-message results
	TotalCount int          // Total number of messages checked
	FoundCount int          // Number of messages found
	ErrorCount int          // Number of errors (excluding not found)
}

// AllAvailable returns true if all messages are available.
func (r *BatchStatResult) AllAvailable() bool {
	return r.FoundCount == r.TotalCount
}
