package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/puzpuzpuz/xsync/v4"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/utils"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// Fixer handles torrent repair with cascading re-insertion across debrids.
type Fixer struct {
	manager *Manager

	// Track re-insertion attempts and failures
	failedToReinsert   *xsync.Map[string, struct{}]      // infohash:debrid -> failed completely
	inFlightRepairs    *xsync.Map[string, *FixerRequest] // infohash -> repair request
	providerOrder      []string                          // Order of providers to try (from config)
	maxReinsertRetries int
}

// FixerRequest tracks an ongoing repair operation.
type FixerRequest struct {
	InfoHash         string
	CurrentDebrid    string
	AttemptedDebrids []string
	StartedAt        time.Time
	LastAttempt      time.Time

	// done is closed once result is set, releasing every concurrent waiter.
	done   chan struct{}
	result *FixResult
}

// wait blocks until the in-flight repair finishes and returns its result.
func (r *FixerRequest) wait(ctx context.Context, name string) (*FixResult, error) {
	timer := time.NewTimer(fixerWaitTimeout)
	defer timer.Stop()
	select {
	case <-r.done:
		return r.result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("repair timeout for %s", name)
	}
}

// FixResult is the result of a fix operation.
type FixResult struct {
	Success       bool
	NewDebrid     string
	Error         error
	AttemptsCount int
}

const (
	// fixerWaitTimeout bounds how long a caller waits on another caller's repair.
	fixerWaitTimeout = 5 * time.Minute
	// fixerMaxReinsertRetries is how many times each debrid is retried.
	fixerMaxReinsertRetries = 2
)

// NewFixer creates a new Fixer instance.
func NewFixer(manager *Manager) *Fixer {
	// GetReader debrid order from config
	cfg := manager.config
	debridOrder := make([]string, 0, len(cfg.Debrids))
	for _, d := range cfg.Debrids {
		debridOrder = append(debridOrder, d.Name)
	}

	return &Fixer{
		manager:            manager,
		failedToReinsert:   xsync.NewMap[string, struct{}](),
		inFlightRepairs:    xsync.NewMap[string, *FixerRequest](),
		providerOrder:      debridOrder,
		maxReinsertRetries: fixerMaxReinsertRetries,
	}
}

// ReinsertEntry retries a torrent through the configured debrid providers.
func (m *Manager) ReinsertEntry(ctx context.Context, entry *storage.Entry) error {
	if m.fixer == nil {
		return fmt.Errorf("fixer not initialized")
	}
	result, err := m.fixer.FixTorrent(ctx, entry, false)
	if err != nil {
		return err
	}
	if !result.Success {
		return errors.New("failed to re-insert torrent")
	}
	return nil
}

// FixTorrent attempts to fix a broken torrent by re-inserting across debrids
// Strategy:
// 1. Try to re-insert on current active debrid, except if skipCurrent is true
// 2. If fails, cascade through other debrids in config order
// 3. Skip debrids where torrent already exists (unless they're also broken)
// 4. Mark as completely failed if all debrids fail.
func (f *Fixer) FixTorrent(ctx context.Context, entry *storage.Entry, skipCurrent bool) (*FixResult, error) {
	if entry == nil {
		return nil, fmt.Errorf("entry is nil")
	}
	if !entry.CanBeFixed() {
		return &FixResult{
			Success:       false,
			Error:         fmt.Errorf("entry %s cannot be fixed", entry.Name),
			AttemptsCount: 0,
		}, nil
	}
	req := &FixerRequest{
		InfoHash:         entry.InfoHash,
		CurrentDebrid:    entry.ActiveProvider,
		AttemptedDebrids: make([]string, 0),
		StartedAt:        time.Now(),
		LastAttempt:      time.Now(),
		done:             make(chan struct{}),
	}
	// LoadOrStore so two callers can never both start a repair; everyone else
	// waits on the owner's done channel.
	if inFlight, loaded := f.inFlightRepairs.LoadOrStore(entry.InfoHash, req); loaded {
		return inFlight.wait(ctx, entry.Name)
	}
	defer f.inFlightRepairs.Delete(entry.InfoHash)

	result, err := f.runRepair(ctx, entry, skipCurrent, req)
	req.result = result
	close(req.done)
	return result, err
}

// runRepair tries each debrid in order until one accepts the entry, marking
// the entry bad when all of them fail.
func (f *Fixer) runRepair(
	ctx context.Context,
	entry *storage.Entry,
	skipCurrent bool,
	req *FixerRequest,
) (*FixResult, error) {
	var lastErr error
	totalAttempts := 0

	for _, debridName := range f.buildAttemptOrder(entry, skipCurrent) {
		// Check if entry has been marked as failed to re-insert
		if f.IsFailedToReinsert(entry.InfoHash, debridName) {
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return &FixResult{Success: false, Error: ctxErr, AttemptsCount: totalAttempts}, ctxErr
		}

		req.AttemptedDebrids = append(req.AttemptedDebrids, debridName)
		req.LastAttempt = time.Now()

		f.manager.logger.Trace().
			Str("debrid", debridName).
			Str("infohash", entry.InfoHash).
			Str("name", entry.Name).
			Int("attempt", totalAttempts+1).
			Msg("Attempting re-insertion")

		// Force a fresh submit only for the broken active provider; for any other
		// debrid, let MoveTorrent reuse an existing valid placement if present.
		reinsert := debridName == entry.ActiveProvider
		success, err := f.MoveTorrent(entry, debridName, reinsert)
		totalAttempts++

		if success {
			f.manager.logger.Info().
				Str("debrid", debridName).
				Str("name", entry.Name).
				Str("infohash", entry.InfoHash).
				Msg("Successfully re-inserted entry")
			f.ResetFailureState(entry.InfoHash)
			return &FixResult{Success: true, NewDebrid: debridName, AttemptsCount: totalAttempts}, nil
		}

		lastErr = err
		f.failedToReinsert.Store(failureKey(entry.InfoHash, debridName), struct{}{})
	}
	if lastErr == nil {
		lastErr = errors.New("no debrid left to try")
	}

	// All debrids failed - mark as completely failed
	f.manager.logger.Error().
		Err(lastErr).
		Str("infohash", entry.InfoHash).
		Int("attempts", totalAttempts).
		Msg("All re-insertion attempts failed")

	f.failedToReinsert.Store(entry.InfoHash, struct{}{})

	// Mark entry as bad
	entry.Bad = true
	entry.UpdatedAt = time.Now()
	_ = f.manager.AddOrUpdate(entry, func(_ *storage.Entry) {
		f.manager.InvalidateEntryCache()
		if err := f.manager.RefreshMount(); err != nil {
			f.manager.logger.Error().Err(err).Msg("Mount refresh failed")
		}
	})

	result := &FixResult{
		Success:       false,
		Error:         fmt.Errorf("all re-insertion attempts failed: %w", lastErr),
		AttemptsCount: totalAttempts,
	}
	return result, result.Error
}

// MoveTorrent attempts to re-insert a torrent on a specific debrid.
func (f *Fixer) MoveTorrent(entry *storage.Entry, debridName string, reinsert bool) (bool, error) {
	// Check if entry can be moved
	if entry == nil {
		return false, fmt.Errorf("entry is nil")
	}
	if !entry.CanBeMoved() {
		return false, fmt.Errorf("entry %s cannot be moved", entry.Name)
	}

	defer func() {
		// Save to storage
		_ = f.manager.AddOrUpdate(entry, nil) // No need to refresh mounts
	}()

	client := f.manager.ProviderClient(debridName)
	if client == nil {
		return false, fmt.Errorf("debrid client %s not found", debridName)
	}

	// Prefer activating an existing, completed placement on the target debrid
	// before re-submitting the magnet. Skipped when reinsert=true — e.g. the
	// current active provider just failed and its placement is presumed stale.
	if !reinsert && activateExistingPlacement(entry, debridName) {
		return true, nil
	}

	// Only replace the old torrent on the same provider. Other placements stay valid.
	var oldID string
	if source, ok := entry.Providers[debridName]; ok && source != nil && debridName == entry.ActiveProvider {
		oldID = source.ID
	}

	newDebridTorrent, err := f.submitReplacement(client, entry)
	if err != nil {
		return false, err
	}
	f.adoptPlacement(entry, debridName, newDebridTorrent)

	// Delete old entry from debrid if different ID
	if oldID != "" && oldID != newDebridTorrent.Id {
		go func() {
			_ = client.DeleteTorrent(oldID)
		}()
	}

	return true, nil
}

// activateExistingPlacement switches entry to a completed placement it
// already has on debridName.
func activateExistingPlacement(entry *storage.Entry, debridName string) bool {
	target, ok := entry.Providers[debridName]
	if !ok || target == nil || target.ID == "" || target.Status != types.TorrentStatusDownloaded {
		return false
	}
	if err := entry.ActivatePlacement(debridName); err != nil {
		return false // fall through to a fresh submit
	}
	entry.Bad = false
	entry.UpdatedAt = time.Now()
	return true
}

// submitReplacement submits entry's magnet to client and returns the new
// placement once every file has a link or ID; a failed placement is deleted.
func (f *Fixer) submitReplacement(client debrid.Client, entry *storage.Entry) (*types.Torrent, error) {
	magnet, err := utils.GetMagnetInfo(entry.Magnet, f.manager.store.Get().AlwaysRmTrackerUrls)
	if err != nil {
		magnet = utils.ConstructMagnet(entry.InfoHash, entry.Name)
	}
	if magnet == nil || magnet.Link == "" {
		return nil, fmt.Errorf("failed to construct magnet for entry %s", entry.Name)
	}

	newDebridTorrent, err := client.SubmitMagnet(&types.Torrent{
		Name:             entry.Name,
		Magnet:           magnet,
		InfoHash:         entry.InfoHash,
		Size:             entry.Size,
		Files:            make(map[string]types.File),
		DownloadUncached: false,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to submit magnet: %w", err)
	}
	if newDebridTorrent == nil || newDebridTorrent.Id == "" {
		return nil, fmt.Errorf("failed to submit magnet: empty entry")
	}

	newDebridTorrent.DownloadUncached = false
	newDebridTorrent, err = client.CheckStatus(newDebridTorrent)
	if errors.Is(err, customerror.TorrentNotCachedError) {
		f.manager.hearsay.ReportAdd(client.Config().Provider, entry.InfoHash, false)
	}
	if err != nil {
		if newDebridTorrent != nil && newDebridTorrent.Id != "" {
			_ = client.DeleteTorrent(newDebridTorrent.Id)
		}
		return nil, fmt.Errorf("failed to check status: %w", err)
	}
	f.manager.hearsay.ReportAdd(
		client.Config().Provider,
		entry.InfoHash,
		newDebridTorrent.Status == types.TorrentStatusDownloaded,
	)

	if len(newDebridTorrent.Files) == 0 {
		_ = client.DeleteTorrent(newDebridTorrent.Id)
		return nil, fmt.Errorf("no files in entry after re-insertion")
	}
	for _, file := range newDebridTorrent.GetFiles() {
		if file.Link == "" && file.Id == "" {
			_ = client.DeleteTorrent(newDebridTorrent.Id)
			return nil, fmt.Errorf("empty link/id for file %s", file.Name)
		}
	}
	return newDebridTorrent, nil
}

// adoptPlacement records a new placement on entry, revives its files and
// makes debridName active.
func (f *Fixer) adoptPlacement(entry *storage.Entry, debridName string, placement *types.Torrent) {
	addedOn := placement.Added
	if addedOn.IsZero() {
		addedOn = time.Now()
	}
	_ = entry.AddTorrentProvider(placement)
	if entry.Files == nil {
		entry.Files = make(map[string]*storage.File)
	}
	for _, file := range placement.GetFiles() {
		if existing, exists := entry.Files[file.Name]; exists {
			existing.Size = file.Size
			existing.ByteRange = file.ByteRange
			existing.Deleted = false
			existing.InfoHash = entry.InfoHash
			existing.AddedOn = addedOn
			continue
		}
		entry.Files[file.Name] = &storage.File{
			Name:      file.Name,
			Size:      file.Size,
			ByteRange: file.ByteRange,
			Deleted:   false,
			InfoHash:  entry.InfoHash,
			AddedOn:   addedOn,
		}
	}
	if err := entry.ActivatePlacement(debridName); err != nil {
		f.manager.logger.Warn().Err(err).Msg("failed to activate placement")
	}
	entry.Bad = false
	entry.UpdatedAt = time.Now()
}

// buildAttemptOrder creates the order of debrids to attempt re-insertion
// Priority: current active debrid first, then others in config order
// If skipCurrent is true, current active debrid is skipped.
func (f *Fixer) buildAttemptOrder(torrent *storage.Entry, skipCurrent bool) []string {
	order := make([]string, 0, len(f.providerOrder))

	// AddOrUpdate other debrids in config order
	for _, debridName := range f.providerOrder {
		if debridName == torrent.ActiveProvider && skipCurrent {
			continue
		}
		order = append(order, debridName)
	}

	return order
}

// IsFailedToReinsert checks if a torrent has been marked as failed to re-insert.
func (f *Fixer) IsFailedToReinsert(infohash, debrid string) bool {
	_, failed := f.failedToReinsert.Load(failureKey(infohash, debrid))
	return failed
}

// ResetFailureState clears every failure recorded for a torrent, including the
// per-debrid ones, so a later repair may try those debrids again.
func (f *Fixer) ResetFailureState(infohash string) {
	prefix := failureKey(infohash, "")
	f.failedToReinsert.Range(func(key string, _ struct{}) bool {
		if key == infohash || strings.HasPrefix(key, prefix) {
			f.failedToReinsert.Delete(key)
		}
		return true
	})
}

func failureKey(infohash, debrid string) string {
	return infohash + ":" + debrid
}
