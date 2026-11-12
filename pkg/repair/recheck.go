package repair

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"uuid"

	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// logForce records that an operator overrode the breaker, and refuses to let a webhook do the same.
func (r *Service) logForce(opts RecheckOptions, what string) RecheckOptions {
	if opts.Source == SourceWebhook {
		if opts.Force {
			r.logger.Warn().
				Str("caller", opts.Caller).
				Str("action", what).
				Msg("Repair: a webhook cannot force; ignored")
		}
		opts.Force = false
		return opts
	}
	opts.Source = SourceOperator
	if opts.Force && opts.Fix {
		r.logger.Info().Str("caller", opts.Caller).Str("action", what).Msg("Repair: operator forced past the breaker")
	}
	return opts
}

// RecheckEntry starts a background probe for one entry.
func (r *Service) RecheckEntry(
	ctx context.Context,
	entryName string,
	opts RecheckOptions,
) (*storage.EntryHealth, error) {
	opts = r.logForce(opts, "recheck-entry "+entryName)
	fix := opts.Fix
	if entryName == "" {
		return nil, errors.New("entry name is empty")
	}
	h, _ := r.storage.GetEntryHealth(entryName)
	if h != nil && h.ActiveRunID != "" {
		return nil, fmt.Errorf("entry is being probed by run %s", h.ActiveRunID)
	}

	item, err := r.storage.GetEntryItem(entryName)
	if err != nil || item == nil {
		return nil, fmt.Errorf("entry %q not found", entryName)
	}

	runID := "recheck-" + uuid.New().String()
	c := &candidate{name: entryName, item: item}

	if ctx == nil {
		ctx = r.parentCtx
	}
	r.runWG.Go(func() {
		if fix {
			r.attachArrContext(ctx, c)
		}
		heal := newErrorCache()
		nzb := r.nzbProber()
		result := r.probeEntry(ctx, runID, c, heal, nzb, RunOptions{MissingProviderIsBroken: true}, fix)
		if result == nil || !fix || result.health.Status != storage.HealthBroken {
			return
		}
		policy := opts.policy()
		files := result.health.BrokenFiles
		if policy.confirm {
			files = result.confirmed
		}
		batch := newProbeBatch(true)
		batch.noteProbed(result)
		batch.add(result.health, files, result.wasHeld)
		r.healBatch(ctx, &storage.RepairRun{ID: runID}, batch, policy)
	})

	if h == nil {
		h = &storage.EntryHealth{EntryName: entryName}
	}
	h.Status = storage.HealthRepairing
	h.ActiveRunID = runID
	return h, nil
}

// RecheckMedia starts a background probe for entries resolved through Arr.
func (r *Service) RecheckMedia(
	ctx context.Context,
	arrName, mediaID string,
	opts RecheckOptions,
) (*storage.RepairRun, error) {
	opts = r.logForce(opts, "recheck-media "+arrName+"/"+mediaID)
	mediaID = strings.TrimSpace(mediaID)
	if mediaID == "" {
		return nil, errors.New("media_id is required")
	}

	arrs, err := r.resolveArrsForMedia(arrName)
	if err != nil {
		return nil, err
	}

	run := newManualRun(storage.RepairStageSelecting, fmt.Sprintf("media:%s/%s", arrName, mediaID))
	return r.startManualRun(ctx, run, func(runCtx context.Context) {
		r.executeRecheckMedia(runCtx, run, arrs, arrName, mediaID, opts)
	})
}

func (r *Service) executeRecheckMedia(
	ctx context.Context,
	run *storage.RepairRun,
	arrs []arr.Arr,
	arrName, mediaID string,
	opts RecheckOptions,
) {
	candidates := make(map[string]*candidate)
	var lastErr error
	for _, a := range arrs {
		if ctx.Err() != nil {
			break
		}
		sub, err := r.collectArrMediaCandidates(ctx, a, mediaID)
		if err != nil {
			lastErr = err
			r.logger.Trace().Err(err).Str("arr", a.Name).Str("media_id", mediaID).Msg("RecheckMedia: GetMedia failed")
			continue
		}
		mergeCandidates(candidates, sub)
		if arrName == "" && len(sub) > 0 {
			break
		}
	}

	if len(candidates) == 0 {
		msg := fmt.Sprintf("media id %q resolved no entries", mediaID)
		if lastErr != nil {
			msg += " (last error: " + lastErr.Error() + ")"
		}
		r.finalizeRun(run, storage.RepairRunCompleted, msg, "")
		return
	}

	run.Stats.Candidates = len(candidates)
	run.Stage = storage.RepairStageProbing
	r.saveRun(run)

	heal := newErrorCache()
	mediaNames := slices.Sorted(maps.Keys(candidates))
	err := r.probeAndHealCandidates(
		ctx, run, candidates, mediaNames, heal, RunOptions{}, opts.Fix, opts.policy(),
	)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			r.finalizeRun(run, storage.RepairRunCancelled, "", "context cancelled during probing")
			return
		}
		r.finalizeRun(run, storage.RepairRunFailed, err.Error(), "")
		return
	}
	if ctx.Err() != nil {
		r.finalizeRun(run, storage.RepairRunCancelled, "", "context cancelled during repair")
		return
	}

	r.finalizeRun(run, storage.RepairRunCompleted, "", "")
	r.logger.Info().
		Str("run_id", run.ID).
		Str("arr", arrName).
		Str("media_id", mediaID).
		Int("candidates", run.Stats.Candidates).
		Int("broken", run.Stats.Broken).
		Int("repaired", run.Stats.Repaired).
		Bool("fix", opts.Fix).
		Msg("RecheckMedia: completed")
}

func (r *Service) resolveArrsForMedia(arrName string) ([]arr.Arr, error) {
	if arrName != "" {
		instance, ok := r.arrs.Get(arrName)
		if !ok {
			return nil, fmt.Errorf("arr %q not found", arrName)
		}
		if !instance.Reachable() {
			return nil, fmt.Errorf("arr %q is not configured", arrName)
		}
		if instance.SkipRepair {
			return nil, fmt.Errorf("arr %q has skip_repair set", arrName)
		}
		return []arr.Arr{instance}, nil
	}
	all := r.eligibleArrs(nil)
	if len(all) == 0 {
		return nil, errors.New("no eligible arrs configured")
	}
	return all, nil
}
