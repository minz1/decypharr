package repair

import (
	"context"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// probeAndHealCandidates probes every candidate, then decides and carries out the heals of the whole batch.
func (r *Service) probeAndHealCandidates(
	ctx context.Context,
	run *storage.RepairRun,
	candidates map[string]*candidate,
	names []string,
	heal *errorCache,
	opts RunOptions,
	autoRepair bool,
	policy healPolicy,
) error {
	batch, err := r.probeCandidates(ctx, run, candidates, names, heal, opts, autoRepair, policy)
	if err != nil {
		return err
	}
	if autoRepair {
		r.healBatch(ctx, run, batch, policy)
	}
	return nil
}

// probeCandidates probes every candidate and collects what is broken into a batch; nothing heals here. The batch
// is returned even when the context ends, holding what was probed so far.
func (r *Service) probeCandidates(
	ctx context.Context,
	run *storage.RepairRun,
	candidates map[string]*candidate,
	names []string,
	heal *errorCache,
	opts RunOptions,
	autoRepair bool,
	policy healPolicy,
) (*probeBatch, error) {
	var runMu sync.Mutex
	batch := newProbeBatch(true)
	batch.sweep = policy.sweep

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

			result := r.probeEntry(gctx, run.ID, c, heal, nzb, opts, autoRepair)
			if result == nil {
				c.item = nil
				c.contentMap = nil
				return nil
			}
			h := result.health
			batch.noteProbed(result)

			if autoRepair && h.Status == storage.HealthBroken {
				files := h.BrokenFiles
				if policy.confirm {
					files = result.confirmed
				}
				batch.add(h, files, result.wasHeld)
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
			case storage.HealthRepairing, storage.HealthStale, storage.HealthHeld:
				// A finished probe never leaves these; nothing to count.
			}
			r.saveRun(run)
			runMu.Unlock()

			c.item = nil
			c.contentMap = nil
			return nil
		})
	}
	return batch, g.Wait()
}
