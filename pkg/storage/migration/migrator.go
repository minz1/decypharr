package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/logger"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// Migrator handles migration from cache JSON files to unified bbolt system.
type Migrator struct {
	storage    *storage.Storage
	cacheDir   string
	backupPath string
	logger     zerolog.Logger
	mu         sync.RWMutex
	cancelFunc context.CancelFunc
	ctx        context.Context
}

// New creates a new migrator.
func New(storage *storage.Storage) *Migrator {
	cacheDir := filepath.Join(config.GetMainPath(), "cache")
	backupPath := filepath.Join(config.GetMainPath(), "backups")

	return &Migrator{
		storage:    storage,
		cacheDir:   cacheDir,
		backupPath: backupPath,
		logger:     logger.New("migrator"),
	}
}

// Start starts the migration process from cache files.
func (m *Migrator) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Load cache torrents
	cachedTorrents, err := m.loadCacheTorrents()
	if err != nil {
		return fmt.Errorf("failed to load cache torrents: %w", err)
	}

	// Initialize migration status
	status := &storage.SystemMigrationStatus{
		Running:   true,
		Total:     len(cachedTorrents),
		Completed: 0,
		Errors:    0,
		StartedAt: time.Now(),
		UpdatedAt: time.Now(),
		ErrorList: []string{},
	}

	if saveMigrationStatusErr := m.storage.SaveMigrationStatus(status); saveMigrationStatusErr != nil {
		return fmt.Errorf("failed to save migration status: %w", saveMigrationStatusErr)
	}

	// Start migration in background
	ctx, cancel := context.WithCancel(context.Background())
	m.ctx = ctx
	m.cancelFunc = cancel

	m.runMigration(ctx, cachedTorrents)

	return nil
}

// Stop stops the migration process.
func (m *Migrator) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cancelFunc != nil {
		m.cancelFunc()
		m.cancelFunc = nil
	}

	// Update status
	status, err := m.storage.GetMigrationStatus()
	if err != nil {
		return err
	}

	status.Running = false
	status.UpdatedAt = time.Now()

	return m.storage.SaveMigrationStatus(status)
}

// GetStatus returns the current migration status.
func (m *Migrator) GetStatus() (*storage.SystemMigrationStatus, error) {
	return m.storage.GetMigrationStatus()
}

// GetStats returns migration statistics.
func (m *Migrator) GetStats() (map[string]any, error) {
	cachedTorrents, err := m.loadCacheTorrents()
	if err != nil {
		return nil, err
	}

	managedCount, err := m.storage.Count()
	if err != nil {
		return nil, err
	}

	// Count total cache files
	totalCacheFiles := 0
	for _, list := range cachedTorrents {
		totalCacheFiles += len(list)
	}

	return map[string]any{
		"cache_torrents":     len(cachedTorrents),
		"cache_files":        totalCacheFiles,
		"managed_count":      managedCount,
		"multi_debrid_count": m.countMultiDebrid(cachedTorrents),
	}, nil
}

// countMultiDebrid counts how many torrents exist on multiple debrids.
func (m *Migrator) countMultiDebrid(torrents map[string][]*storage.CachedTorrent) int {
	count := 0
	for _, list := range torrents {
		if len(list) > 1 {
			count++
		}
	}
	return count
}

// runMigration performs the actual migration.
func (m *Migrator) runMigration(ctx context.Context, cachedTorrents map[string][]*storage.CachedTorrent) {
	m.logger.Info().Msg("Starting migration from cache files")

	status, _ := m.storage.GetMigrationStatus()

	for infohash, cachedList := range cachedTorrents {
		select {
		case <-ctx.Done():
			m.logger.Info().Msg("Migration stopped by user")
			return
		default:
		}

		// Check if already migrated
		exists, err := m.storage.Exists(infohash)
		if err != nil {
			m.logger.Error().Err(err).Str("infohash", infohash).Msg("Failed to check existence")
			status.Errors++
			continue
		}

		if exists {
			status.Completed++
			status.UpdatedAt = time.Now()
			_ = m.storage.SaveMigrationStatus(status)
			continue
		}

		// Merge cache torrents from multiple debrids
		managed, err := m.mergeCachedTorrents(cachedList)
		if err != nil {
			m.logger.Error().Err(err).
				Str("infohash", infohash).
				Int("count", len(cachedList)).
				Msg("Failed to merge cached torrents")
			status.Errors++
			status.ErrorList = append(status.ErrorList, fmt.Sprintf("Failed to merge %s: %v", infohash, err))
			continue
		}

		// Save to new storage
		if addOrUpdateErr := m.storage.AddOrUpdate(managed); addOrUpdateErr != nil {
			m.logger.Error().Err(addOrUpdateErr).Str("infohash", infohash).Msg("Failed to add managed torrent")
			status.Errors++
			status.ErrorList = append(
				status.ErrorList,
				fmt.Sprintf("Failed to add %s: %v", managed.Name, addOrUpdateErr),
			)
			continue
		}
		status.Completed++
		status.UpdatedAt = time.Now()

		// Update status every 10 torrents
		if status.Completed%10 == 0 {
			if saveMigrationStatusErr := m.storage.SaveMigrationStatus(status); saveMigrationStatusErr != nil {
				m.logger.Error().Err(saveMigrationStatusErr).Msg("Failed to update migration status")
			}
		}
	}

	// Final status update
	status.Running = false
	status.UpdatedAt = time.Now()
	_ = m.storage.SaveMigrationStatus(status)

	m.logger.Info().
		Int("total", status.Total).
		Int("completed", status.Completed).
		Int("errors", status.Errors).
		Msg("Migration completed")
}

// cachePercentScale converts the cache's 0-100 progress to a placement's 0-1.
const cachePercentScale = 100.0

// loadCacheTorrents loads all torrents from cache directories and groups by infohash.
func (m *Migrator) loadCacheTorrents() (map[string][]*storage.CachedTorrent, error) {
	// Map: infohash -> []*CachedTorrent (multiple debrids)
	torrentsByHash := make(map[string][]*storage.CachedTorrent)

	// Check if cache directory exists
	if _, err := os.Stat(m.cacheDir); os.IsNotExist(err) {
		return torrentsByHash, nil
	}

	// Read all debrid subdirectories
	debridDirs, err := os.ReadDir(m.cacheDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read cache directory: %w", err)
	}

	for _, debridDir := range debridDirs {
		if debridDir.IsDir() {
			m.loadDebridCache(debridDir.Name(), torrentsByHash)
		}
	}

	return torrentsByHash, nil
}

// loadDebridCache adds every readable cache file of one debrid directory to
// torrentsByHash. Unreadable files are logged and skipped.
func (m *Migrator) loadDebridCache(debridName string, torrentsByHash map[string][]*storage.CachedTorrent) {
	debridPath := filepath.Join(m.cacheDir, debridName)

	// Read all JSON files in this debrid directory
	files, err := os.ReadDir(debridPath)
	if err != nil {
		m.logger.Error().Err(err).Str("path", debridPath).Msg("Failed to read debrid directory")
		return
	}

	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		cached, ok := m.readCachedTorrent(filepath.Join(debridPath, file.Name()))
		if !ok {
			continue
		}

		// Ensure debrid field is set
		if cached.Debrid == "" {
			cached.Debrid = debridName
		}

		// Group by infohash
		torrentsByHash[cached.InfoHash] = append(torrentsByHash[cached.InfoHash], cached)
	}
}

// readCachedTorrent parses one cache file. It reports false, after logging,
// for a file that cannot be read, decoded, or has no info hash.
func (m *Migrator) readCachedTorrent(filePath string) (*storage.CachedTorrent, bool) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		m.logger.Error().Err(err).Str("file", filePath).Msg("Failed to read cache file")
		return nil, false
	}

	var cached storage.CachedTorrent
	if unmarshalErr := json.Unmarshal(data, &cached); unmarshalErr != nil {
		m.logger.Error().Err(unmarshalErr).Str("file", filePath).Msg("Failed to unmarshal cache file")
		return nil, false
	}

	// Validate required fields
	if cached.InfoHash == "" {
		m.logger.Warn().Str("file", filePath).Msg("Cache file missing info_hash, skipping")
		return nil, false
	}
	return &cached, true
}

// mergeCachedTorrents merges multiple cache entries (from different debrids) into a single Entry.
func (m *Migrator) mergeCachedTorrents(cachedList []*storage.CachedTorrent) (*storage.Entry, error) {
	if len(cachedList) == 0 {
		return nil, fmt.Errorf("empty cached list")
	}

	// Use first as base
	base := cachedList[0]
	managed := base.ToManagedTorrent()

	// AddOrUpdate placements from other debrids
	for _, other := range cachedList[1:] {
		// Check if placement already exists for this debrid+infohash combo
		if _, exists := managed.Providers[other.Debrid]; exists {
			continue
		}
		mergeCachedPlacement(managed, other)
	}

	// Activate the most complete placement
	m.activateBestPlacement(managed)

	return managed, nil
}

// mergeCachedPlacement adds other's debrid placement to managed, along with any
// files managed does not know yet.
func mergeCachedPlacement(managed *storage.Entry, other *storage.CachedTorrent) {
	// Parse timestamp
	addedAt, err := time.Parse(time.RFC3339, other.AddedOn)
	if err != nil {
		addedAt = time.Now()
	}

	// Determine placement status
	status := debridTypes.TorrentStatusDownloaded
	if other.Bad {
		status = debridTypes.TorrentStatusError
	}

	// Create placement
	placement := &storage.ProviderEntry{
		Provider: other.Debrid,
		ID:       other.ID,
		AddedAt:  addedAt,
		Status:   status,
		Progress: other.Progress / cachePercentScale,
		Files:    make(map[string]*storage.ProviderFile),
	}

	// Set downloaded timestamp if complete
	if other.IsComplete && other.Status == "downloaded" {
		downloadedAt := addedAt // Use added time as approximation
		placement.DownloadedAt = &downloadedAt
	}

	managed.Providers[other.Debrid] = placement

	// Merge files - add any files not in the base and populate placement files
	for fileName, file := range other.Files {
		// AddOrUpdate to global files if not exists
		if _, exists := managed.Files[fileName]; !exists {
			managed.Files[fileName] = &storage.File{
				Name:      fileName,
				Size:      file.Size,
				ByteRange: file.ByteRange,
				Deleted:   file.Deleted,
				InfoHash:  other.InfoHash, // Track which torrent this file came from
				AddedOn:   addedAt,
			}
		}

		// AddOrUpdate placement-specific file data
		placement.Files[fileName] = &storage.ProviderFile{
			Id:   file.Id,
			Link: file.Link,
			Path: file.Path,
		}
	}

	// Update size if other has larger size
	if other.Bytes > managed.Bytes {
		managed.Bytes = other.Bytes
		managed.Size = other.Bytes
	}
}

// activateBestPlacement finds and activates the first placement that is completed.
func (m *Migrator) activateBestPlacement(torrent *storage.Entry) {
	for debrid, placement := range torrent.Providers {
		if placement.Status == debridTypes.TorrentStatusDownloaded {
			torrent.ActiveProvider = debrid
			return
		}
	}
}
