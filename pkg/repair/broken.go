package repair

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"uuid"

	"github.com/puzpuzpuz/xsync/v4"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

func (r *Service) collectBrokenHealths(
	names []string,
	requireArrFile bool,
) (*xsync.Map[string, *storage.EntryHealth], int) {
	wanted := make(map[string]struct{}, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			wanted[n] = struct{}{}
		}
	}

	healths := xsync.NewMap[string, *storage.EntryHealth]()
	_ = r.storage.ForEachEntryHealth(func(h *storage.EntryHealth) error {
		if h == nil || h.Status != storage.HealthBroken {
			return nil
		}
		if _, ok := wanted[h.EntryName]; len(wanted) > 0 && !ok {
			return nil
		}
		if requireArrFile && !hasArrBrokenFile(h) {
			return nil
		}
		healths.Store(h.EntryName, h)
		return nil
	})
	return healths, len(wanted)
}

// hasArrBrokenFile reports whether any broken file carries enough Arr identity
// for a reacquisition.
func hasArrBrokenFile(h *storage.EntryHealth) bool {
	for _, bf := range h.BrokenFiles {
		if bf.ArrName != "" && bf.InfoHash != "" && bf.FileName != "" {
			return true
		}
	}
	return false
}

func (r *Service) markBrokenHealthCleared(h *storage.EntryHealth, at time.Time) {
	if h == nil {
		return
	}
	if _, err := r.storage.GetEntryItem(h.EntryName); err != nil {
		_ = r.storage.DeleteEntryHealth(h.EntryName)
		return
	}
	h.Status = storage.HealthUnknown
	h.BrokenFiles = nil
	h.FailureReason = ""
	h.LastRepairAt = at
	h.Dirty = false
	h.DirtyReason = ""
	h.NextCheckDueAt = time.Time{}
	r.saveHealth(h)
}

func isAlreadyClearedFileError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "file does not exist") ||
		strings.Contains(msg, "file is deleted")
}

// FixBroken repairs persisted broken entries without probing them again.
func (r *Service) FixBroken(ctx context.Context, names []string) (*storage.RepairRun, error) {
	healths, wantedCount := r.collectBrokenHealths(names, true)
	if healths.Size() == 0 {
		return nil, errors.New("no fixable broken entries")
	}
	return r.startBrokenRun(ctx, healths, brokenRunSource("fix-broken", wantedCount), "repair", r.repairBroken)
}

// ClearBroken removes persisted broken files without calling Arr.
func (r *Service) ClearBroken(ctx context.Context, names []string) (*storage.RepairRun, error) {
	healths, wantedCount := r.collectBrokenHealths(names, false)
	if healths.Size() == 0 {
		return nil, errors.New("no broken files to clear")
	}
	return r.startBrokenRun(ctx, healths, brokenRunSource("clear-broken", wantedCount), "clear", r.clearBroken)
}

// startBrokenRun runs apply over already-probed broken healths as a manual
// run. verb names the work in the cancellation reason.
func (r *Service) startBrokenRun(
	ctx context.Context,
	healths *xsync.Map[string, *storage.EntryHealth],
	source, verb string,
	apply func(context.Context, *storage.RepairRun, *xsync.Map[string, *storage.EntryHealth]),
) (*storage.RepairRun, error) {
	run := newManualRun(storage.RepairStageRepairing, source)
	run.Stats.Candidates = healths.Size()
	return r.startManualRun(ctx, run, func(runCtx context.Context) {
		apply(runCtx, run, healths)
		if runCtx.Err() != nil {
			r.finalizeRun(run, storage.RepairRunCancelled, "", "context cancelled during "+verb)
			return
		}
		r.finalizeRun(run, storage.RepairRunCompleted, "", "")
		r.logger.Info().
			Str("run_id", run.ID).
			Str("source", source).
			Int("candidates", run.Stats.Candidates).
			Int("repaired", run.Stats.Repaired).
			Int("cleared", run.Stats.Cleared).
			Int("failed", run.Stats.RepairFailed).
			Msg("Broken-entry run completed")
	})
}

// brokenRunSource labels a fix/clear run by how many entries were requested.
func brokenRunSource(action string, wantedCount int) string {
	if wantedCount > 0 {
		return fmt.Sprintf("%s:%d", action, wantedCount)
	}
	return action + ":all"
}

// newManualRun builds the record of a manually triggered run.
func newManualRun(stage storage.RepairRunStage, source string) *storage.RepairRun {
	return &storage.RepairRun{
		ID:        uuid.New().String(),
		Trigger:   storage.RepairTriggerManual,
		Status:    storage.RepairRunRunning,
		Stage:     stage,
		StartedAt: time.Now(),
		Source:    source,
	}
}

// startManualRun claims the single active-run slot for run, persists it, and
// runs work in the background with a context Stop and StopRun cancel. It fails
// when another run is active or the run record cannot be saved. A nil ctx
// falls back to the service's parent context.
func (r *Service) startManualRun(
	ctx context.Context,
	run *storage.RepairRun,
	work func(context.Context),
) (*storage.RepairRun, error) {
	if ctx == nil {
		ctx = r.parentCtx
	}
	r.mu.Lock()
	if r.activeRunID != "" {
		id := r.activeRunID
		r.mu.Unlock()
		return nil, fmt.Errorf("repair already running (run %s)", id)
	}
	runCtx, cancel := context.WithCancel(ctx)
	r.activeRunID = run.ID
	r.cancelRun = cancel
	r.mu.Unlock()

	if err := r.storage.SaveRepairRun(run); err != nil {
		r.mu.Lock()
		r.activeRunID = ""
		r.cancelRun = nil
		r.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("failed to persist repair run: %w", err)
	}

	r.runWG.Go(func() {
		defer func() {
			r.mu.Lock()
			if r.activeRunID == run.ID {
				r.activeRunID = ""
				r.cancelRun = nil
			}
			r.mu.Unlock()
			cancel()
		}()
		work(runCtx)
	})
	return run, nil
}

func (r *Service) clearBroken(
	ctx context.Context,
	run *storage.RepairRun,
	healths *xsync.Map[string, *storage.EntryHealth],
) {
	now := time.Now()
	healths.Range(func(_ string, h *storage.EntryHealth) bool {
		if ctx != nil && ctx.Err() != nil {
			return false
		}
		if h == nil {
			return true
		}
		if len(h.BrokenFiles) == 0 {
			r.markBrokenHealthCleared(h, now)
			run.Stats.Cleared++
			r.saveRun(run)
			return true
		}

		remaining := make([]storage.BrokenFile, 0, len(h.BrokenFiles))
		for _, bf := range h.BrokenFiles {
			if err := r.backend.RemoveTorrentFile(bf.EntryName, bf.FileName); err != nil {
				if isAlreadyClearedFileError(err) {
					run.Stats.Cleared++
					r.saveRun(run)
					continue
				}
				r.logger.Warn().
					Err(err).
					Str("entry", bf.EntryName).
					Str("file", bf.FileName).
					Msg("ClearBroken: failed to remove broken file from mount")
				run.Stats.RepairFailed++
				remaining = append(remaining, bf)
				continue
			}
			run.Stats.Cleared++
			r.saveRun(run)
		}

		h.LastRepairAt = now
		h.BrokenFiles = remaining
		if len(remaining) == 0 {
			r.markBrokenHealthCleared(h, now)
			return true
		}

		h.Status = storage.HealthBroken
		h.FailureReason = topReason(remaining)
		r.saveHealth(h)
		return true
	})
}
