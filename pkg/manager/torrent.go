package manager

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

func (m *Manager) syncTorrents(ctx context.Context) {
	// First time syncTorrents debrid -> storage
	m.logger.Info().
		Int("debrids", m.clients.Size()).
		Msg("Performing initial sync of torrents from debrid clients...")
	var wg sync.WaitGroup
	m.clients.Range(func(name string, client debrid.Client) bool {
		wg.Go(func() {
			if err := m.refreshTorrents(ctx, name, client); err != nil {
				m.logger.Error().Err(err).Str("debrid", name).Msg("Initial torrent sync failed")
			}
			m.InvalidateEntryCache()
		})
		return true
	})
	wg.Wait()
	m.logger.Info().
		Msg("Initial sync of torrents from debrid clients completed")
}

// Refresh configuration constants.
const (
	refreshBatchSize      = 500
	refreshWriteBatchSize = 50
	refreshFlushInterval  = 3 * time.Second
	refreshMaxWorkers     = 50 // Capped to avoid overwhelming debrid APIs
	refreshMinWorkers     = 5
	// refreshTorrentsPerWorker scales refresh workers with the batch size.
	refreshTorrentsPerWorker = 10
	refreshDeleteWorkers     = 10
	refreshWorkChanBuffer    = 100
	refreshBatchChanBuffer   = 50
)

// errSyncSkipped reports a provider torrent that cannot be synced yet: its
// provider client is gone or its files are still missing download links.
var errSyncSkipped = errors.New("torrent not ready to sync")

// refreshTorrents refreshes torrents from a specific debrid service.
// Returns an error if the refresh fails.
func (m *Manager) refreshTorrents(ctx context.Context, provider string, debridClient debrid.Client) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Use singleflight to prevent concurrent refreshes for the same debrid
	_, err, _ := m.refreshSG.Do(provider, func() (any, error) {
		return nil, m.doRefreshTorrents(ctx, provider, debridClient)
	})

	return err
}

// doRefreshTorrents performs the actual refresh logic.
func (m *Manager) doRefreshTorrents(_ context.Context, provider string, debridClient debrid.Client) error {
	remote, err := debridClient.GetTorrents()
	if err != nil {
		m.logger.Error().Err(err).Str("debrid", provider).Msg("Failed to get remote")
		return err
	}

	if len(remote) == 0 {
		m.logger.Debug().Str("debrid", provider).Msg("No remote found")
		return nil
	}

	// Build map of current remote by infohash
	remoteTorrentsByHash := make(map[string]*types.Torrent, len(remote))
	for _, t := range remote {
		old, exists := remoteTorrentsByHash[t.InfoHash]
		if !exists {
			remoteTorrentsByHash[t.InfoHash] = t
		}
		if exists && t.Added.After(old.Added) {
			remoteTorrentsByHash[t.InfoHash] = t
		}
	}

	previousMisses, _ := m.providerMisses.Load(provider)

	// Detect changes by streaming through cached entries
	changes, err := m.detectTorrentChanges(provider, remoteTorrentsByHash, previousMisses)
	if err != nil {
		return err
	}
	newTorrents, torrentsToUpdate := changes.fetch, changes.update

	// Handle deletions
	m.handleTorrentDeletions(provider, changes.delete)
	m.providerMisses.Store(provider, changes.misses)

	// Batch update torrents with changed placements (run concurrently)
	var updateWg sync.WaitGroup
	if len(torrentsToUpdate) > 0 {
		updateWg.Add(1)
		go func(torrents []*storage.Entry) {
			defer updateWg.Done()
			if batchAddOrUpdateErr := m.storage.BatchAddOrUpdate(torrents); batchAddOrUpdateErr != nil {
				m.logger.Error().Err(batchAddOrUpdateErr).Msg("Failed to batch update remote")
			}
		}(torrentsToUpdate)
	}

	// Process new torrents
	if len(newTorrents) > 0 {
		if processNewTorrentsErr := m.processNewTorrents(provider, newTorrents); processNewTorrentsErr != nil {
			m.logger.Error().Err(processNewTorrentsErr).Str("debrid", provider).Msg("Failed to process new torrents")
		}
	}

	// Wait for concurrent update to finish
	updateWg.Wait()

	return nil
}

// torrentChanges is what one provider listing changes in storage.
type torrentChanges struct {
	fetch  []*types.Torrent // new or changed on the provider; re-fetched in full
	update []*storage.Entry // lost this provider but keep others
	delete []*storage.Entry // lost their last provider
	// misses are the infohashes this listing lacks although the entry is
	// placed on the provider. A placement is dropped only on a second
	// consecutive miss, so one partial listing never acts.
	misses map[string]struct{}
}

// classify records how entry changes given the provider's current listing
// of it (nil when the provider no longer lists it).
func (c *torrentChanges) classify(provider string, entry *storage.Entry, current *types.Torrent, missedBefore bool) {
	placement, onProvider := entry.Providers[provider]
	switch {
	case !onProvider:
		if current != nil {
			c.fetch = append(c.fetch, current)
		}
	case current == nil:
		if c.misses == nil {
			c.misses = make(map[string]struct{})
		}
		c.misses[entry.InfoHash] = struct{}{}
		if !missedBefore {
			return
		}
		entry.RemoveProvider(provider, nil)
		if len(entry.Providers) == 0 {
			c.delete = append(c.delete, entry)
		} else {
			c.update = append(c.update, entry)
		}
	case placement.NeedsUpdate(current):
		// The listing lacks full metadata (files, downloadedAt), so re-fetch;
		// processNewTorrents only updates the placement of known entries.
		c.fetch = append(c.fetch, current)
	}
}

// detectTorrentChanges streams through cached entries and detects what changed.
func (m *Manager) detectTorrentChanges(
	provider string,
	remoteTorrentsByHash map[string]*types.Torrent,
	previousMisses map[string]struct{},
) (torrentChanges, error) {
	var changes torrentChanges
	cachedInfoHashes := make(map[string]bool, len(remoteTorrentsByHash))

	err := m.storage.ForEachBatch(refreshBatchSize, func(batch []*storage.Entry) error {
		for _, entry := range batch {
			cachedInfoHashes[entry.InfoHash] = true
			_, missedBefore := previousMisses[entry.InfoHash]
			changes.classify(provider, entry, remoteTorrentsByHash[entry.InfoHash], missedBefore)
		}
		return nil
	})
	if err != nil {
		m.logger.Error().Err(err).Msg("Failed to stream cached remote")
		return torrentChanges{}, err
	}

	// Check for brand new torrents (not in cache at all)
	for infohash, t := range remoteTorrentsByHash {
		if !cachedInfoHashes[infohash] {
			changes.fetch = append(changes.fetch, t)
		}
	}
	return changes, nil
}

// handleTorrentDeletions processes torrent deletions concurrently.
func (m *Manager) handleTorrentDeletions(provider string, torrentsToDelete []*storage.Entry) {
	if len(torrentsToDelete) == 0 {
		return
	}

	var deleteWg sync.WaitGroup
	deleteChan := make(chan *storage.Entry, len(torrentsToDelete))

	deleteWorkers := min(refreshDeleteWorkers, len(torrentsToDelete))
	for range deleteWorkers {
		deleteWg.Go(func() {
			for entry := range deleteChan {
				m.recoverDroppedEntry(provider, entry)
				if err := m.storage.Delete(entry.InfoHash); err != nil {
					m.logger.Error().Err(err).Str("infohash", entry.InfoHash).Msg("Failed to delete torrent")
				}
			}
		})
	}

	for _, entry := range torrentsToDelete {
		deleteChan <- entry
	}
	close(deleteChan)
	deleteWg.Wait()
}

// processNewTorrents processes new torrents with worker pool and batch writing.
func (m *Manager) processNewTorrents(provider string, newTorrents []*types.Torrent) error {
	workChan := make(chan *types.Torrent, min(refreshWorkChanBuffer, len(newTorrents)))
	batchChan := make(chan *storage.Entry, refreshBatchChanBuffer)
	errChan := make(chan error, 1) // Buffer for first error

	var processWg sync.WaitGroup
	var batchWg sync.WaitGroup
	var processed atomic.Int64
	totalTorrents := len(newTorrents)

	// Batch writer goroutine
	batchWg.Go(func() {
		m.runBatchWriter(batchChan, errChan)
	})

	// Scale workers based on torrent count, but cap to avoid overwhelming APIs
	workers := min(refreshMaxWorkers, max(refreshMinWorkers, len(newTorrents)/refreshTorrentsPerWorker))

	for range workers {
		processWg.Go(func() {
			for t := range workChan {
				mt, err := m.processSyncTorrent(t)
				switch {
				case errors.Is(err, errSyncSkipped):
				case err != nil:
					m.logger.Error().Err(err).Str("debrid", provider).Msgf("Failed to process torrent %s", t.ID)
				default:
					batchChan <- mt
				}
				count := processed.Add(1)
				if count%50 == 0 {
					m.logger.Debug().
						Str("debrid", provider).
						Msgf("Processed %d / %d new torrents", count, totalTorrents)
				}
			}
		})
	}

	// Send torrents to workers
	for _, t := range newTorrents {
		workChan <- t
	}

	close(workChan)
	processWg.Wait()
	close(batchChan)
	batchWg.Wait()

	// Check if batch writer encountered an error
	select {
	case err := <-errChan:
		return err
	default:
		return nil
	}
}

// runBatchWriter collects entries and writes them in batches.
func (m *Manager) runBatchWriter(batchChan <-chan *storage.Entry, errChan chan<- error) {
	batch := make([]*storage.Entry, 0, refreshWriteBatchSize)
	ticker := time.NewTicker(refreshFlushInterval)
	defer ticker.Stop()

	var writeErr error
	flushBatch := func() {
		if len(batch) == 0 || writeErr != nil {
			return
		}
		if err := m.storage.BatchAddOrUpdate(batch); err != nil {
			m.logger.Error().Err(err).Msg("Failed to batch write remote")
			writeErr = err
			// Send first error to channel (non-blocking)
			select {
			case errChan <- err:
			default:
			}
		}
		// Clear slice
		for i := range batch {
			batch[i] = nil
		}
		batch = batch[:0]
	}

	for {
		select {
		case t, ok := <-batchChan:
			if !ok {
				flushBatch()
				return
			}
			batch = append(batch, t)
			if len(batch) >= refreshWriteBatchSize {
				flushBatch()
			}
		case <-ticker.C:
			flushBatch()
		}
	}
}

// processSyncTorrent processes a single torrent and returns it for batched writing.
func (m *Manager) processSyncTorrent(t *types.Torrent) (*storage.Entry, error) {
	// GetReader the debrid client
	client := m.ProviderClient(t.Debrid)
	if client == nil {
		return nil, errSyncSkipped
	}

	// Check if files are complete - only make API call if needed
	needsUpdate := len(t.Files) == 0 || !isComplete(t.Files)
	if needsUpdate {
		// This is the main bottleneck - API call per torrent
		// Consider: Could we batch UpdateTorrent calls? Depends on debrid API
		if err := client.UpdateTorrent(t); err != nil {
			return nil, err
		}

		// Re-check completion after update
		if !isComplete(t.Files) {
			return nil, errSyncSkipped
		}
	}

	addedOn := t.Added
	if addedOn.IsZero() {
		addedOn = time.Now()
	}

	// Check if we have an existing managed torrent
	// Note: This is a database read per torrent - could be optimized with batch reads
	// or an in-memory cache, but storage.GetReader is likely fast (indexed by InfoHash)
	mt, err := m.storage.Get(t.InfoHash)
	if err != nil {
		mt = newSyncedEntry(t, addedOn)
	}

	// Populate global Files metadata (only if empty)
	if len(mt.Files) == 0 {
		for _, f := range t.GetFiles() {
			mt.Files[f.Name] = &storage.File{
				Name:      f.Name,
				Size:      f.Size,
				ByteRange: f.ByteRange,
				Deleted:   f.Deleted,
				InfoHash:  t.InfoHash,
				AddedOn:   addedOn,
			}
		}
	}

	// AddOrUpdate or update placement
	placement := mt.AddTorrentProvider(t)
	placement.Progress = t.Progress
	if t.Status == types.TorrentStatusDownloaded {
		downloadedAt := addedOn
		placement.DownloadedAt = &downloadedAt
	}

	// If this is the first placement or the only one, make it active
	if (mt.ActiveProvider == "" || len(mt.Providers) == 1) && t.Status == types.TorrentStatusDownloaded {
		_ = mt.ActivatePlacement(t.Debrid)
	}

	// confirm everything is complete
	if validateErr := mt.Validate(); validateErr != nil {
		m.logger.Warn().
			Err(validateErr).
			Str("infohash", t.InfoHash).
			Str("name", mt.Name).
			Msg("Validation failed for torrent, marking as bad")
	}

	return mt, nil
}

// newSyncedEntry builds the managed entry for a provider torrent that storage
// does not know yet.
func newSyncedEntry(t *types.Torrent, addedOn time.Time) *storage.Entry {
	magnet := t.Magnet
	if magnet == nil || magnet.Link == "" {
		magnet = utils.ConstructMagnet(t.InfoHash, t.Name)
	}
	size := t.Size
	if size == 0 {
		size = t.Bytes
	}
	return &storage.Entry{
		Protocol:         config.ProtocolTorrent,
		InfoHash:         t.InfoHash,
		Name:             t.Name,
		OriginalFilename: t.OriginalFilename,
		Size:             size,
		Bytes:            size,
		Magnet:           magnet.Link,
		ActiveProvider:   t.Debrid,
		Providers:        make(map[string]*storage.ProviderEntry),
		Files:            make(map[string]*storage.File),
		Status:           t.Status,
		Progress:         t.Progress,
		Speed:            t.Speed,
		Seeders:          t.Seeders,
		IsComplete:       len(t.Files) > 0,
		Bad:              false,
		AddedOn:          addedOn,
		CreatedAt:        addedOn,
		UpdatedAt:        time.Now(),
	}
}

// refreshTorrent refreshes a single torrent from its active debrid.
func (m *Manager) refreshTorrent(infohash string) (*storage.Entry, error) {
	torrent, err := m.storage.Get(infohash)
	if err != nil {
		return nil, err
	}

	if torrent.ActiveProvider == "" {
		return torrent, nil
	}

	client := m.ProviderClient(torrent.ActiveProvider)
	if client == nil {
		return torrent, nil
	}

	placement := torrent.GetActiveProvider()
	if placement == nil {
		return torrent, nil
	}

	// GetReader updated torrent info from debrid
	debridTorrent, err := client.GetTorrent(placement.ID)
	if err != nil {
		return nil, err
	}

	entry, err := m.processSyncTorrent(debridTorrent)
	if errors.Is(err, errSyncSkipped) {
		// Nothing new to store; callers dereference the result, so hand back
		// the stored entry rather than nil.
		return torrent, nil
	}
	if err != nil {
		return nil, err
	}
	if addOrUpdateErr := m.storage.AddOrUpdate(entry); addOrUpdateErr != nil {
		return nil, addOrUpdateErr
	}
	return entry, nil
}

// refreshDebridDownloadLinks refreshes download links for a specific debrid service.
func (m *Manager) refreshDebridDownloadLinks(ctx context.Context, debridName string, client debrid.Client) {
	select {
	case <-ctx.Done():
		return
	default:
	}

	if client == nil {
		m.logger.Warn().Str("debrid", debridName).Msg("Provider client is nil, skipping download link refresh")
		return
	}

	if err := client.RefreshDownloadLinks(); err != nil {
		m.logger.Error().Err(err).Str("debrid", debridName).Msg("Failed to refresh download links")
	}
}

// isComplete checks if all files in a torrent have download links.
func isComplete(files map[string]types.File) bool {
	if len(files) == 0 {
		return false
	}
	for _, file := range files {
		if file.Link == "" {
			return false
		}
	}
	return true
}
