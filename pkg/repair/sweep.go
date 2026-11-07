package repair

import (
	"context"
	"errors"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type fileResult struct {
	name     string
	infoHash string
	protocol config.Protocol
	provider string // the active provider probed, "usenet" for an NZB
	healthy  bool
	fresh    bool // healthy by a fresh read check, the only kind that may clear a bad mark
	broken   bool
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
	policy := healPolicy{confirm: true}
	batch, err := r.probeCandidates(ctx, run, due, names, heal, opts, autoRepair, policy)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			r.finishCancelledRepairSweep(ctx, run, stopState, autoRepair, "context cancelled during probing", batch)
			return
		}
		log.Error().Err(err).Msg("Sweep: probing failed")
		r.finalizeRun(run, storage.RepairRunFailed, err.Error(), "")
		return
	}
	if ctx.Err() != nil {
		r.finishCancelledRepairSweep(ctx, run, stopState, autoRepair, "context cancelled after probing", batch)
		return
	}

	if autoRepair {
		run.Stage = storage.RepairStageRepairing
		r.saveRun(run)
		r.healBatch(ctx, run, batch, policy)
		if ctx.Err() != nil {
			r.finishCancelledRepairSweep(ctx, run, stopState, autoRepair, "context cancelled during repair", batch)
			return
		}
	}

	r.finalizeRun(run, storage.RepairRunCompleted, "", "")
	log.Info().
		Int("probed", run.Stats.Probed).
		Int("broken", run.Stats.Broken).
		Int("healthy", run.Stats.Healthy).
		Int("held", run.Stats.Held).
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
	batch *probeBatch,
) {
	stopped := stopState != nil && stopState.get()
	if !stopped {
		r.finalizeRun(run, storage.RepairRunCancelled, "", reason)
		return
	}

	log := r.logger.With().Str("run_id", run.ID).Logger()
	log.Info().Bool("auto_repair", autoRepair).Msg("Repair sweep: stop schedule fired; finishing run")

	if autoRepair && batch != nil && len(batch.pendingNames()) > 0 {
		repairCtx, cancel := context.WithTimeout(detachedRepairContext(ctx, r.parentCtx), repairStopFinalRepairTimeout)
		defer cancel()

		run.Stage = storage.RepairStageRepairing
		r.saveRun(run)
		// The final heal is judged by the breaker like any other: a stop does not lift the outage rule.
		r.healBatch(repairCtx, run, batch, healPolicy{confirm: true})
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
