package manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/utils"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// Fixer handles torrent repair with cascading re-insertion across debrids.
type Fixer struct {
	manager *Manager

	repairs            repairTable
	providerOrder      []string
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
		if r.result == nil {
			return nil, fmt.Errorf("repair of %s ended without a result", name)
		}
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
	req := f.newRequest(entry)
	if inFlight, owner := f.repairs.begin(entry.InfoHash, req); !owner {
		return inFlight.wait(ctx, entry.Name)
	}
	result, err := f.runRepair(ctx, entry.Clone(), skipCurrent, req)
	f.finish(entry.InfoHash, req, result)
	return result, err
}

func (f *Fixer) newRequest(entry *storage.Entry) *FixerRequest {
	return &FixerRequest{
		InfoHash:         entry.InfoHash,
		CurrentDebrid:    entry.ActiveProvider,
		AttemptedDebrids: make([]string, 0),
		StartedAt:        time.Now(),
		LastAttempt:      time.Now(),
		done:             make(chan struct{}),
	}
}

// finish stores a repair's result, releases its waiters and clears it from the table.
func (f *Fixer) finish(key string, req *FixerRequest, result *FixResult) {
	req.result = result
	close(req.done)
	f.repairs.end(key)
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
		if f.repairs.failed(entry.InfoHash, debridName, time.Now()) {
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
		result, err := f.MoveTorrent(entry, debridName, reinsert)
		if err == nil {
			err = f.manager.applyRepairResult(f.manager.work.Context(), entry.InfoHash, result)
		}
		totalAttempts++

		if errors.Is(err, errRepairSuperseded) || errors.Is(err, errPlacementChanged) {
			return &FixResult{Success: false, Error: err, AttemptsCount: totalAttempts}, err
		}
		if err == nil {
			f.manager.logger.Info().
				Str("debrid", debridName).
				Str("name", entry.Name).
				Str("infohash", entry.InfoHash).
				Msg("Successfully re-inserted entry")
			f.repairs.reset(entry.InfoHash)
			return &FixResult{Success: true, NewDebrid: debridName, AttemptsCount: totalAttempts}, nil
		}

		lastErr = err
		// A call cut by the stop is not a failed heal: no memo, no give-up.
		if stopErr := f.workStopped(); stopErr != nil {
			return &FixResult{Success: false, Error: stopErr, AttemptsCount: totalAttempts}, stopErr
		}
		f.manager.logger.Warn().
			Err(err).
			Str("debrid", debridName).
			Str("infohash", entry.InfoHash).
			Str("name", entry.Name).
			Msg("re-insert attempt failed")
		f.repairs.fail(entry.InfoHash, debridName, time.Now())
	}
	// Giving up fails the grab in the Arr, so a stopping manager must never reach it.
	if stopErr := f.workStopped(); stopErr != nil {
		return &FixResult{Success: false, Error: stopErr, AttemptsCount: totalAttempts}, stopErr
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

	f.manager.giveUpOnEntry(entry)

	result := &FixResult{
		Success:       false,
		Error:         fmt.Errorf("all re-insertion attempts failed: %w", lastErr),
		AttemptsCount: totalAttempts,
	}
	return result, result.Error
}

// workStopped is task.ErrStopped once the manager is stopping, else nil. Only the manager's stop makes a failed
// provider call a stop: the ctx FixTorrent is handed ends with a config save or a closed player, and a call that
// failed while the manager runs is a real failure whoever asked for the heal.
func (f *Fixer) workStopped() error { return f.manager.stopErr() }

// MoveTorrent decides how to put a torrent on a specific debrid: it activates a completed placement the entry
// already has there, or submits the magnet again. It reads entry and writes nothing; the caller applies the result.
// Its provider calls run on the work ctx, so it takes no ctx of its own.
func (f *Fixer) MoveTorrent(entry *storage.Entry, debridName string, reinsert bool) (RepairResult, error) {
	if entry == nil {
		return RepairResult{}, fmt.Errorf("entry is nil")
	}
	if !entry.CanBeMoved() {
		return RepairResult{}, fmt.Errorf("entry %s cannot be moved", entry.Name)
	}

	client := f.manager.ProviderClient(debridName)
	if client == nil {
		return RepairResult{}, fmt.Errorf("debrid client %s not found", debridName)
	}

	var seenID string
	if seen := entry.Providers[debridName]; seen != nil {
		seenID = seen.ID
	}

	// Prefer activating an existing, completed placement on the target debrid
	// before re-submitting the magnet. Skipped when reinsert=true — e.g. the
	// current active provider just failed and its placement is presumed stale.
	if !reinsert && canActivateExisting(entry, debridName) {
		return RepairResult{Provider: debridName, SeenID: seenID}, nil
	}

	// Only replace the old torrent on the same provider. Other placements stay valid.
	var old storage.ProviderEntry
	if source, ok := entry.Providers[debridName]; ok && source != nil && debridName == entry.ActiveProvider {
		old = *source
	}

	newDebridTorrent, err := f.submitReplacement(client, debridName, entry)
	if err != nil {
		return RepairResult{}, err
	}
	result := RepairResult{Provider: debridName, Placement: newDebridTorrent, SeenID: seenID}

	// Delete the old torrent from the provider if it has a different ID, unless another entry (the other seasons
	// of a pack) still lists it.
	if old.ID != "" && old.ID != newDebridTorrent.ID {
		result.Release = &placementRelease{
			provider: debridName, id: old.ID, grab: entry.InfoHash, delete: true, slot: old.HoldsSlot(), library: true,
		}
	}
	return result, nil
}

// canActivateExisting reports whether entry has a completed placement on debridName it can switch to.
func canActivateExisting(entry *storage.Entry, debridName string) bool {
	target, ok := entry.Providers[debridName]
	if !ok || target == nil || target.ID == "" || target.Status != types.TorrentStatusDownloaded {
		return false
	}
	return entry.Clone().ActivatePlacement(debridName) == nil
}

// submitReplacement submits entry's magnet to client and returns the new
// placement once every file has a link or ID; a failed placement is deleted.
func (f *Fixer) submitReplacement(
	client debrid.Client,
	debridName string,
	entry *storage.Entry,
) (*types.Torrent, error) {
	magnet, err := utils.GetMagnetInfo(entry.Magnet, f.manager.store.Get().AlwaysRmTrackerUrls)
	if err != nil {
		magnet = utils.ConstructMagnet(entry.InfoHash, entry.Name)
	}
	if magnet == nil || magnet.Link == "" {
		return nil, fmt.Errorf("failed to construct magnet for entry %s", entry.Name)
	}

	// The provider calls run on the work ctx, never the handed one: a config save or a closed player must not cut a
	// heal and read as a failed one.
	work := f.manager.work.Context()
	newDebridTorrent, err := client.SubmitMagnet(work, &types.Torrent{
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
	if newDebridTorrent == nil || newDebridTorrent.ID == "" {
		return nil, fmt.Errorf("failed to submit magnet: empty entry")
	}

	newDebridTorrent.DownloadUncached = false
	newDebridTorrent, err = client.CheckStatus(work, newDebridTorrent)
	if errors.Is(err, customerror.ErrTorrentNotCached) {
		f.manager.hearsay.ReportAdd(client.Config().Provider, entry.InfoHash, false)
	}
	if err != nil {
		// A stop leaves the submitted torrent at the provider, next to the old one: the check never finished, so
		// nothing says it is unwanted.
		if newDebridTorrent != nil && newDebridTorrent.ID != "" && !stopped(work) {
			_ = f.manager.discardSubmitted(work, debridName, newDebridTorrent.ID, entry.InfoHash)
		}
		return nil, fmt.Errorf("failed to check status: %w", err)
	}
	f.manager.hearsay.ReportAdd(
		client.Config().Provider,
		entry.InfoHash,
		newDebridTorrent.Status == types.TorrentStatusDownloaded,
	)

	if len(newDebridTorrent.Files) == 0 {
		_ = f.manager.discardSubmitted(work, debridName, newDebridTorrent.ID, entry.InfoHash)
		return nil, fmt.Errorf("no files in entry after re-insertion")
	}
	for _, file := range newDebridTorrent.GetFiles() {
		if file.Link == "" && file.ID == "" {
			_ = f.manager.discardSubmitted(work, debridName, newDebridTorrent.ID, entry.InfoHash)
			return nil, fmt.Errorf("empty link/id for file %s", file.Name)
		}
	}
	return newDebridTorrent, nil
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
