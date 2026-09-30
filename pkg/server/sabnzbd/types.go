package sabnzbd

import (
	"fmt"
	"time"

	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// SABnzbd API response types based on official documentation

var (
	Version = "4.5.0"
)

// QueueResponse represents the queue status response.
type QueueResponse struct {
	Queue   Queue  `json:"queue"`
	Status  bool   `json:"status"`
	Version string `json:"version"`
}

// Queue represents the download queue.
type Queue struct {
	Version string      `json:"version"`
	Slots   []QueueSlot `json:"slots"`
}

// QueueSlot represents a download in the queue.
type QueueSlot struct {
	Status       string   `json:"status"`
	Index        int      `json:"index"`
	Password     string   `json:"password"`
	AvgAge       string   `json:"avg_age"`
	TimeAdded    int64    `json:"time_added"`
	Script       string   `json:"script"`
	DirectUnpack string   `json:"direct_unpack"`
	Mb           string   `json:"mb"`
	MBLeft       string   `json:"mbleft"`
	MBMissing    string   `json:"mbmissing"`
	Size         string   `json:"size"`
	SizeLeft     string   `json:"sizeleft"`
	Filename     string   `json:"filename"`
	Labels       []string `json:"labels"`
	Priority     string   `json:"priority"`
	Cat          string   `json:"cat"`
	TimeLeft     string   `json:"timeleft"`
	Percentage   string   `json:"percentage"`
	NzoID        string   `json:"nzo_id"`
	Unpackopts   string   `json:"unpackopts"`
}

// HistoryResponse represents the history response.
type HistoryResponse struct {
	History History `json:"history"`
}

// History represents the download history.
type History struct {
	Version string        `json:"version"`
	Paused  bool          `json:"paused"`
	Slots   []HistorySlot `json:"slots"`
}

// HistorySlot represents a completed download.
type HistorySlot struct {
	Status      string `json:"status"`
	Name        string `json:"name"`
	NZBName     string `json:"nzb_name"`
	NzoID       string `json:"nzo_id"`
	Category    string `json:"category"`
	FailMessage string `json:"fail_message"`
	Bytes       int64  `json:"bytes"`
	Storage     string `json:"storage"`
}

// StageLog represents processing stages.
type StageLog struct {
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// VersionResponse represents version information.
type VersionResponse struct {
	Version string `json:"version"`
}

// StatusResponse represents general status.
type StatusResponse struct {
	Status bool   `json:"status"`
	Error  string `json:"error,omitempty"`
}

// FullStatusResponse represents the full status response with queue and history.
type FullStatusResponse struct {
	Queue   Queue   `json:"queue"`
	History History `json:"history"`
	Status  bool    `json:"status"`
	Version string  `json:"version"`
}

// AddNZBRequest represents the request to add an NZB.
type AddNZBRequest struct {
	Name     string `json:"name"`
	Cat      string `json:"cat"`
	Script   string `json:"script"`
	Priority string `json:"priority"`
	PP       string `json:"pp"`
	Password string `json:"password"`
	NZBData  []byte `json:"nzb_data"`
	URL      string `json:"url"`
}

// AddNZBResponse represents the response when adding an NZB.
type AddNZBResponse struct {
	Status bool     `json:"status"`
	NzoIDs []string `json:"nzo_ids"`
	Error  string   `json:"error,omitempty"`
}

// API Mode constants.
const (
	ModeQueue      = "queue"
	ModeHistory    = "history"
	ModeConfig     = "config"
	ModeGetConfig  = "get_config"
	ModeAddURL     = "addurl"
	ModeAddFile    = "addfile"
	ModeVersion    = "version"
	ModePause      = "pause"
	ModeResume     = "resume"
	ModeDelete     = "delete"
	ModeShutdown   = "shutdown"
	ModeRestart    = "restart"
	ModeGetCats    = "get_cats"
	ModeGetScripts = "get_scripts"
	ModeGetFiles   = "get_files"
	ModeRetry      = "retry"
	ModeStatus     = "status"
	ModeFullStatus = "fullstatus"
)

// Status constants.
const (
	StatusQueued      = "Queued"
	StatusPaused      = "Paused"
	StatusDownloading = "Downloading"
	StatusProcessing  = "Processing"
	StatusCompleted   = "Completed"
	StatusFailed      = "Failed"
	StatusGrabbing    = "Grabbing"
	StatusPropagating = "Propagating"
	StatusVerifying   = "Verifying"
	StatusRepairing   = "Repairing"
	StatusExtracting  = "Extracting"
	StatusMoving      = "Moving"
	StatusRunning     = "Running"
)

// Priority constants.
const (
	PriorityForced = "2"
	PriorityHigh   = "1"
	PriorityNormal = "0"
	PriorityLow    = "-1"
	PriorityStop   = "-2"
)

// NZB represents an NZB download in SABnzbd format (similar to qbit's Torrent).
type NZB struct {
	NzoID        string   `json:"nzo_id"`        // Unique NZB identifier
	Name         string   `json:"name"`          // NZB name
	Filename     string   `json:"filename"`      // Original filename
	Size         int64    `json:"size"`          // Total size in bytes
	SizeMB       int64    `json:"mb"`            // Size in MB
	Percentage   float64  `json:"percentage"`    // Download progress (0-100)
	MBLeft       int64    `json:"mbleft"`        // MB remaining
	TimeLeft     string   `json:"timeleft"`      // Time remaining (HH:MM:SS format)
	Status       string   `json:"status"`        // Current status (Downloading, Paused, etc.)
	Category     string   `json:"cat"`           // Category
	Priority     string   `json:"priority"`      // Priority level
	SavePath     string   `json:"save_path"`     // Download save path
	ContentPath  string   `json:"content_path"`  // Final content path
	Script       string   `json:"script"`        // Post-processing script
	AddedOn      int64    `json:"added_on"`      // Unix timestamp when added
	CompletedOn  int64    `json:"completed_on"`  // Unix timestamp when completed
	FailMessage  string   `json:"fail_message"`  // Error message if failed
	Storage      string   `json:"storage"`       // Storage location
	Files        []File   `json:"files"`         // List of files in NZB
	AvgAge       string   `json:"avg_age"`       // Average age of posts
	Downloaded   int64    `json:"downloaded"`    // Bytes downloaded
	Labels       []string `json:"labels"`        // Labels/tags
	Password     string   `json:"password"`      // Archive password if any
	DirectUnpack bool     `json:"direct_unpack"` // Whether to unpack during download
	Unpackopts   string   `json:"unpackopts"`    // Unpack options
}

// File represents a file within an NZB (similar to qbit's TorrentFile).
type File struct {
	Status   string `json:"status"`        // File status: "finished", "active", or "queued"
	MBLeft   string `json:"mbleft"`        // MB remaining to download
	Mb       string `json:"mb"`            // File size in MB
	Age      string `json:"age"`           // Age of file
	Bytes    string `json:"bytes"`         // Total file size in bytes (as string)
	Filename string `json:"filename"`      // Filename
	NzfID    string `json:"nzf_id"`        // Unique file ID
	Set      string `json:"set,omitempty"` // Optional set name
}

// SABnzbd reports sizes in binary units.
const (
	kb = 1024
	mb = kb * 1024
	gb = mb * 1024
	tb = gb * 1024

	percent = 100
)

// convertToSABnzbdNZB converts a storage.Entry to SABnzbd NZB format.
func convertToSABnzbdNZB(e *storage.Entry) NZB {
	// Calculate MB values
	sizeMB := e.Size / mb
	mbLeft := int64(float64(e.Size) * (1 - e.Progress) / float64(mb))
	downloaded := int64(float64(e.Size) * e.Progress)

	// Calculate time left (simple estimation)
	timeLeft := "0:00:00"
	if e.Speed > 0 && e.Progress < 1.0 {
		bytesLeft := int64(float64(e.Size) * (1 - e.Progress))
		left := time.Duration(bytesLeft/e.Speed) * time.Second
		timeLeft = fmt.Sprintf("%d:%02d:%02d", int64(left.Hours()), int64(left.Minutes())%60, int64(left.Seconds())%60)
	}

	// Map storage state to SABnzbd status
	status := mapStorageStateToSABStatus(e.State)
	if e.Status == debridTypes.TorrentStatusQueued {
		status = StatusQueued
	}

	var completedOn int64
	if e.CompletedAt != nil {
		completedOn = e.CompletedAt.Unix()
	}

	nzb := NZB{
		NzoID:        e.InfoHash,
		Name:         e.Name,
		Filename:     e.OriginalFilename,
		Size:         e.Size,
		SizeMB:       sizeMB,
		Percentage:   e.Progress * percent,
		MBLeft:       mbLeft,
		TimeLeft:     timeLeft,
		Status:       status,
		Category:     e.Category,
		Priority:     PriorityNormal,
		SavePath:     e.SavePath,
		ContentPath:  e.DownloadPath(),
		Script:       "None",
		AddedOn:      e.CreatedAt.Unix(),
		CompletedOn:  completedOn,
		FailMessage:  e.LastError,
		Storage:      e.DownloadPath(),
		Files:        getNZBFiles(e),
		AvgAge:       "0d", // We don't track article age
		Downloaded:   downloaded,
		Labels:       e.Tags,
		DirectUnpack: true,
	}

	return nzb
}

// getNZBFiles converts storage.Entry files to File format.
func getNZBFiles(e *storage.Entry) []File {
	files := make([]File, 0, len(e.Files))

	// Determine file status based on job state
	fileStatus := "queued"
	switch {
	case e.Status == debridTypes.TorrentStatusQueued:
		fileStatus = "queued"
	case e.State == storage.EntryStateDownloading:
		fileStatus = "active"
	case e.State == storage.EntryStatePausedUP:
		fileStatus = "finished"
	}
	idx := 0
	for _, f := range e.Files {
		if f.Deleted {
			continue
		}

		sizeMB := float64(f.Size) / float64(mb)
		// For finished files, mbleft is 0; for active/queued, it's the full size
		mbleft := "0.00"
		if fileStatus != "finished" {
			mbleft = fmt.Sprintf("%.2f", sizeMB)
		}

		files = append(files, File{
			Status:   fileStatus,
			MBLeft:   mbleft,
			Mb:       fmt.Sprintf("%.2f", sizeMB),
			Age:      "0d", // We don't track article age
			Bytes:    fmt.Sprintf("%.2f", float64(f.Size)),
			Filename: f.Name,
			NzfID:    fmt.Sprintf("%s_%d", e.InfoHash, idx),
		})
		idx++
	}

	return files
}

// mapStorageStateToSABStatus maps storage.TorrentState to SABnzbd status.
func mapStorageStateToSABStatus(state storage.TorrentState) string {
	switch state {
	case storage.EntryStateDownloading:
		return StatusDownloading
	case storage.EntryStatePausedDL:
		return StatusPaused
	case storage.EntryStatePausedUP:
		return StatusCompleted
	case storage.EntryStateError:
		return StatusFailed
	default:
		return StatusQueued
	}
}
