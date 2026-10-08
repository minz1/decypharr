package types

import (
	"maps"
	"os"
	"sync"
	"time"

	"github.com/sirrobot01/decypharr/internal/utils"
)

type Torrent struct {
	ID               string          `json:"id"`
	InfoHash         string          `json:"info_hash"`
	Name             string          `json:"name"`
	Filename         string          `json:"filename"`
	OriginalFilename string          `json:"original_filename"`
	Size             int64           `json:"size"`
	Bytes            int64           `json:"bytes"` // Size of only the files that are downloaded
	Magnet           *utils.Magnet   `json:"magnet"`
	Files            map[string]File `json:"files"`
	Status           TorrentStatus   `json:"status"`
	Added            time.Time       `json:"added"`
	Progress         float64         `json:"progress"`
	Speed            int64           `json:"speed"`
	Seeders          int             `json:"seeders"`
	Links            []string        `json:"links"`
	DeletedFiles     []string        `json:"deleted_files"`

	Debrid string `json:"debrid"`

	SizeDownloaded   int64 `json:"-"` // This is used for local download
	DownloadUncached bool  `json:"-"`

	mu sync.Mutex // guards Copy
}

func (t *Torrent) GetSize() int64 {
	if t.Size == 0 {
		return t.Bytes
	}
	return t.Size
}

func (t *Torrent) Copy() *Torrent {
	t.mu.Lock()
	defer t.mu.Unlock()

	newFiles := make(map[string]File, len(t.Files))
	maps.Copy(newFiles, t.Files)

	return &Torrent{
		ID:               t.ID,
		InfoHash:         t.InfoHash,
		Name:             t.Name,
		Filename:         t.Filename,
		OriginalFilename: t.OriginalFilename,
		Size:             t.Size,
		Bytes:            t.Bytes,
		Magnet:           t.Magnet,
		Files:            newFiles,
		Status:           t.Status,
		Added:            t.Added,
		Progress:         t.Progress,
		Speed:            t.Speed,
		Seeders:          t.Seeders,
		Links:            append([]string{}, t.Links...),
		Debrid:           t.Debrid,
	}
}

func (t *Torrent) GetFile(filename string) (File, bool) {
	f, ok := t.Files[filename]
	if !ok {
		return File{}, false
	}
	return f, !f.Deleted
}

func (t *Torrent) GetFiles() []File {
	files := make([]File, 0, len(t.Files))

	for _, f := range t.Files {
		if !f.Deleted {
			files = append(files, f)
		}
	}
	return files
}

type File struct {
	TorrentID    string       `json:"torrent_id"`
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Size         int64        `json:"size"`
	IsRar        bool         `json:"is_rar"`
	ByteRange    *[2]int64    `json:"byte_range,omitempty"`
	Path         string       `json:"path"`
	Link         string       `json:"link"`
	AccountID    string       `json:"account_id"`
	Generated    time.Time    `json:"generated"`
	Deleted      bool         `json:"deleted"`
	DownloadLink DownloadLink `json:"-"`
}

func (t *Torrent) Cleanup(remove bool) {
	if remove {
		err := os.Remove(t.Filename)
		if err != nil {
			return
		}
	}
}

type IngestData struct {
	Debrid string `json:"debrid"`
	Name   string `json:"name"`
	Hash   string `json:"hash"`
	Size   int64  `json:"size"`
}

type LibraryStats struct {
	Total       int `json:"total"`
	Bad         int `json:"bad"`
	ActiveLinks int `json:"active_links"`
}

type Stats struct {
	Profile         *Profile         `json:"profile"`
	Library         LibraryStats     `json:"library"`
	Accounts        []map[string]any `json:"accounts"`
	SpeedTestResult *SpeedTestResult `json:"speed_test_result,omitempty"`
}

type Profile struct {
	Name       string    `json:"name"`
	ID         int64     `json:"id"`
	Username   string    `json:"username"`
	Email      string    `json:"email"`
	Points     int       `json:"points"`
	Type       string    `json:"type"`
	Premium    int64     `json:"premium"`
	Expiration time.Time `json:"expiration"`
}

type DownloadLink struct {
	Debrid       string    `json:"debrid"`
	Token        string    `json:"token"`
	Filename     string    `json:"filename"`
	Link         string    `json:"link"`
	DownloadLink string    `json:"download_link"`
	Generated    time.Time `json:"generated"`
	Size         int64     `json:"size"`
	ID           string    `json:"id"`
	ExpiresAt    time.Time
}

func (dl *DownloadLink) Valid() error {
	if dl.Empty() {
		return ErrEmptyDownloadLink
	}

	// Validate url format
	if !utils.IsValidURL(dl.DownloadLink) {
		return ErrInvalidDownloadLink
	}

	return nil
}

func (dl *DownloadLink) Empty() bool {
	return dl.DownloadLink == ""
}

func (dl *DownloadLink) String() string {
	return dl.DownloadLink
}

// SpeedTestResult holds the result of a debrid provider speed test.
type SpeedTestResult struct {
	Provider  string    `json:"provider"`
	SpeedMBps float64   `json:"speed_mbps"`
	LatencyMs int64     `json:"latency_ms"`
	BytesRead int64     `json:"bytes_read"`
	TestedAt  time.Time `json:"tested_at"`
	Error     string    `json:"error,omitempty"`
}

// ProfileCache memoizes a provider profile and is safe for concurrent use.
// The zero value is empty and ready to use.
type ProfileCache struct {
	mu      sync.Mutex
	profile *Profile
	fetched time.Time
}

// Get returns the cached profile while it is younger than ttl (ttl <= 0 never
// expires) and otherwise refreshes it with fetch. Each caller receives its own
// copy, so callers may modify the result.
func (c *ProfileCache) Get(ttl time.Duration, fetch func() (*Profile, error)) (*Profile, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.profile == nil || (ttl > 0 && time.Since(c.fetched) >= ttl) {
		profile, err := fetch()
		if err != nil {
			return nil, err
		}
		c.profile, c.fetched = profile, time.Now()
	}
	profile := *c.profile
	return &profile, nil
}
