package repair

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type fileResult struct {
	name     string
	infoHash string
	protocol config.Protocol
	healthy  bool
	fresh    bool // healthy by a fresh read check, the only kind that may clear a bad mark
	broken   bool
	deferred bool
	reason   string
}

func (r *Service) executeSweep(
	ctx context.Context,
	run *storage.RepairRun,
	opts RunOptions,
	stopState *repairStopState,
) {
	cfg := r.cfg()
	log := r.logger.With().Str("run_id", run.ID).Logger()

	autoRepair := cfg.AutoRepair
	if opts.AutoRepair != nil {
		autoRepair = *opts.AutoRepair
	}

	log.Info().Str("source", string(cfg.Source)).Msg("Sweep: selecting candidates")
	candidates, err := r.enumerateCandidates(ctx, cfg)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			r.finishCancelledRepairSweep(ctx, run, stopState, autoRepair, "context cancelled during selection", nil)
			return
		}
		log.Error().Err(err).Msg("Sweep: enumeration failed")
		r.finalizeRun(run, storage.RepairRunFailed, err.Error(), "")
		return
	}
	if ctx.Err() != nil {
		r.finishCancelledRepairSweep(ctx, run, stopState, autoRepair, "context cancelled after selection", nil)
		return
	}

	due, skipped := r.filterDueCandidates(candidates, opts)
	protocolScope := r.effectiveProtocolScope(opts)
	due = r.filterCandidatesByProtocol(due, protocolScope)
	run.Stats.Candidates = len(due)
	run.Stats.SkippedFresh = skipped

	names := r.orderCandidatesByLastChecked(due)

	run.Stage = storage.RepairStageProbing
	r.saveRun(run)
	log.Info().
		Int("due", len(due)).
		Int("skipped_fresh", skipped).
		Str("protocol", protocolScope).
		Bool("auto_repair", autoRepair).
		Msg("Sweep: probing")

	heal := newErrorCache()
	err = r.probeAndHealCandidates(ctx, run, due, names, heal, opts, autoRepair, true)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			r.finishCancelledRepairSweep(ctx, run, stopState, autoRepair, "context cancelled during probing", due)
			return
		}
		log.Error().Err(err).Msg("Sweep: probing failed")
		r.finalizeRun(run, storage.RepairRunFailed, err.Error(), "")
		return
	}
	if ctx.Err() != nil {
		r.finishCancelledRepairSweep(ctx, run, stopState, autoRepair, "context cancelled after probing", due)
		return
	}

	r.finalizeRun(run, storage.RepairRunCompleted, "", "")
	log.Info().
		Int("probed", run.Stats.Probed).
		Int("broken", run.Stats.Broken).
		Int("healthy", run.Stats.Healthy).
		Int("repaired", run.Stats.Repaired).
		Int("repair_failed", run.Stats.RepairFailed).
		Msg("Sweep: completed")
}

func (r *Service) finishCancelledRepairSweep(
	ctx context.Context,
	run *storage.RepairRun,
	stopState *repairStopState,
	autoRepair bool,
	reason string,
	candidates map[string]*candidate,
) {
	stopped := stopState != nil && stopState.get()
	if !stopped {
		r.finalizeRun(run, storage.RepairRunCancelled, "", reason)
		return
	}

	log := r.logger.With().Str("run_id", run.ID).Logger()
	log.Info().Bool("auto_repair", autoRepair).Msg("Repair sweep: stop schedule fired; finishing run")

	pending := slices.DeleteFunc(slices.Collect(maps.Values(candidates)), func(c *candidate) bool {
		return len(c.unhealed) == 0
	})
	if autoRepair && len(pending) > 0 {
		repairCtx, cancel := context.WithTimeout(detachedRepairContext(ctx, r.parentCtx), repairStopFinalRepairTimeout)
		defer cancel()

		run.Stage = storage.RepairStageRepairing
		r.saveRun(run)
		var statsMu sync.Mutex
		for _, c := range pending {
			r.healBrokenEntry(repairCtx, run, &statsMu, c.health, c.unhealed)
		}
	}

	run.CancelReason = ""
	r.finalizeRun(run, storage.RepairRunCompleted, "", "stopped by schedule: "+reason)
}

func detachedRepairContext(runCtx, parentCtx context.Context) context.Context {
	if runCtx.Err() == nil {
		return runCtx
	}
	if parentCtx != nil {
		return parentCtx
	}
	return context.Background()
}

func (r *Service) probeAndHealCandidates(
	ctx context.Context,
	run *storage.RepairRun,
	candidates map[string]*candidate,
	names []string,
	heal *errorCache,
	opts RunOptions,
	autoRepair bool,
	confirm bool,
) error {
	var runMu sync.Mutex

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(1, r.workers()))
	nzb := r.nzbProber()

	for _, name := range names {
		c := candidates[name]
		if c == nil {
			continue
		}
		g.Go(func() error {
			if gctx.Err() != nil {
				return gctx.Err()
			}

			h, confirmed := r.probeEntry(gctx, run.ID, c, heal, nzb, opts, autoRepair)
			if h == nil {
				c.item = nil
				c.contentMap = nil
				return nil
			}

			if autoRepair && h.Status == storage.HealthBroken {
				files := h.BrokenFiles
				if confirm {
					files = confirmed
				}
				c.health, c.unhealed = h, r.healBrokenEntry(gctx, run, &runMu, h, files)
			}

			runMu.Lock()
			run.Stats.Probed++
			switch h.Status {
			case storage.HealthHealthy:
				run.Stats.Healthy++
			case storage.HealthBroken:
				run.Stats.Broken++
			case storage.HealthUnknown, storage.HealthUnsupported:
				run.Stats.Unknown++
			case storage.HealthRepairing, storage.HealthStale:
				// A finished probe never leaves these; nothing to count.
			}
			r.saveRun(run)
			runMu.Unlock()

			c.item = nil
			c.contentMap = nil
			return nil
		})
	}
	return g.Wait()
}
