package nntp

import (
	"context"
	"errors"

	"github.com/sirrobot01/decypharr/internal/task"
)

// repairDefaultPercent is used when cfg.Repair.NNTPConnectionPercent is unset.
const repairDefaultPercent = 20

// repairPoolMinWorkers is the floor for the chunk capacity. Even with no
// repair-mode budget configured (BatchStat called by non-repair paths) a
// couple of chunks may run at once, so the caller doesn't have to branch.
const repairPoolMinWorkers = 2

// RepairPool bounds the BatchStat chunks running at once across every
// BatchStat call. Each accepted chunk is a task on the pool's group, holding
// one capacity token while it runs, so M concurrent BatchStat calls share
// the same Capacity() and stopping the group waits for every chunk.
type RepairPool struct {
	// g owns the chunk tasks; stopping it cancels and waits for them.
	g *task.Group
	// tokens holds one entry per running chunk; its capacity is the bound.
	tokens chan struct{}
	// stat runs one chunk's STATs.
	stat func(ctx context.Context, messageIDs []string) ([]StatResult, error)
}

// errRepairPoolClosed is returned by Submit once the pool's group is stopping.
var errRepairPoolClosed = errors.New("repair pool closed")

// errRepairChunkPanicked is the error a chunk reports when its STATs panicked.
var errRepairChunkPanicked = errors.New("repair chunk panicked")

// newRepairPool runs chunks on g, sized to `percent` of the client's total
// provider connections, with a small floor so non-repair BatchStat callers
// still have a couple of chunks available.
func (c *Client) newRepairPool(g *task.Group, percent int) *RepairPool {
	if c == nil {
		return nil
	}
	if percent <= 0 {
		percent = repairDefaultPercent
	}
	const fullPercent = 100
	percent = min(percent, fullPercent)
	total := c.TotalConnections()
	capacity := repairPoolMinWorkers
	if total > 0 {
		sized := (total*percent + fullPercent - 1) / fullPercent
		if sized > capacity {
			capacity = sized
		}
		if capacity > total {
			capacity = total
		}
	}
	return &RepairPool{g: g, tokens: make(chan struct{}, capacity), stat: c.batchStatAcrossProviders}
}

// pickStatBatchSize picks the per-chunk batch size for a BatchStat call.
//
// Default is the ceiling (statBatchSize). For small inputs vs. the
// available capacity, we shrink the chunk size so a single call can
// keep the whole pool busy — otherwise a 100-ID call against a 30-chunk
// pool would only fan out to 2 chunks (chunks=ceil(100/50)) and leave
// 28 slots idle. Floors at minSize so the per-chunk overhead doesn't
// dominate when input is tiny.
//
// overcommit > 1 means we aim for more chunks than slots so a slot
// freed by a fast chunk has a waiting one to take it rather than idling
// while the rest of the pool finishes slower chunks.
func pickStatBatchSize(totalIDs, workers, ceilSize, minSize int) int {
	if workers <= 0 || totalIDs <= 0 {
		return ceilSize
	}
	const overcommit = 3
	want := (totalIDs + workers*overcommit - 1) / (workers * overcommit)
	if want >= ceilSize {
		return ceilSize
	}
	if want < minSize {
		return minSize
	}
	return want
}

// TotalConnections is the sum of MaxConnections across configured providers.
func (c *Client) TotalConnections() int {
	if c == nil {
		return 0
	}
	total := 0
	for _, p := range c.providers {
		if p.MaxConnections > 0 {
			total += p.MaxConnections
		}
	}
	return total
}

// Capacity returns the number of chunks that may run at once.
func (p *RepairPool) Capacity() int {
	if p == nil {
		return 0
	}
	return cap(p.tokens)
}

// Submit waits for a free slot, then runs the chunk as a task. done is
// called exactly once, on the chunk's task, when Submit returns nil. Submit
// returns an error and does NOT call done when the caller's ctx ends before
// a slot frees, or when the pool's group is stopping.
func (p *RepairPool) Submit(ctx context.Context, msgIDs []string, done func([]StatResult, error)) error {
	if p == nil {
		return errRepairPoolClosed
	}
	// A BatchStat that already bailed out must not spawn a chunk.
	if err := ctx.Err(); err != nil {
		return err
	}
	stopping := p.g.Context().Done()
	select {
	case <-stopping:
		return errRepairPoolClosed
	default:
	}
	select {
	case p.tokens <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-stopping:
		return errRepairPoolClosed
	}
	err := p.g.Go("chunk", func(taskCtx context.Context) error {
		p.runChunk(ctx, taskCtx, msgIDs, done)
		return nil
	})
	if err != nil {
		<-p.tokens
		return errRepairPoolClosed
	}
	return nil
}

// runChunk runs one chunk's STATs on a ctx that ends with either the caller's
// ctx or the chunk's task, then gives its slot back and calls done, even when
// the STATs panic.
func (p *RepairPool) runChunk(
	callerCtx, taskCtx context.Context,
	msgIDs []string,
	done func([]StatResult, error),
) {
	runCtx, cancel := context.WithCancel(callerCtx)
	stop := context.AfterFunc(taskCtx, cancel)
	results, err := []StatResult(nil), errRepairChunkPanicked
	defer func() {
		stop()
		cancel()
		<-p.tokens
		if done != nil {
			done(results, err)
		}
	}()
	// The caller may have cancelled while the chunk waited for its task.
	if ctxErr := runCtx.Err(); ctxErr != nil {
		err = ctxErr
		return
	}
	results, err = p.stat(runCtx, msgIDs)
}
