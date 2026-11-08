package reader

import (
	"github.com/sirrobot01/decypharr/internal/buffer"
	"github.com/sirrobot01/decypharr/internal/task"
)

// Pools holds the shared cache budgets for one Usenet service run. The buffer
// pool runs on the "buffers" child of the group it was made on; the group's
// owner stops it, and it is created first, so it stops last.
type Pools struct {
	g       *task.Group
	bufferG *task.Group
	buffers *buffer.Pool
	extents *extentPool
}

// NewPools builds the pools on g. It returns task.ErrStopped (wrapped) if g
// refuses tasks.
func NewPools(g *task.Group, memoryBudget int64) (*Pools, error) {
	bufferG := g.Child("buffers")
	buffers, err := buffer.NewPool(bufferG, buffer.PoolConfig{Name: "usenet", MemoryBudget: memoryBudget})
	if err != nil {
		bufferG.Close()
		return nil, err
	}
	return &Pools{
		g:       g,
		bufferG: bufferG,
		buffers: buffers,
		extents: newExtentPool(memoryBudget),
	}, nil
}

// newReaderGroup makes the group one StreamingReader runs on, a child of the
// pools' group.
func (p *Pools) newReaderGroup() *task.Group { return p.g.Child("reader") }

// BufferGroup is the group the buffer pool runs on. Stopping it closes every
// Buffer still registered.
func (p *Pools) BufferGroup() *task.Group { return p.bufferG }

// Stats reports allocated buffer bytes. These are not process RSS values.
func (p *Pools) Stats() map[string]any {
	blocks := p.buffers.Stats()
	extents := p.extents.stats()
	return map[string]any{
		"memory_in_use":    blocks.MemoryInUse + extents.MemoryInUse,
		"memory_allocated": blocks.MemoryAllocated + extents.MemoryInUse,
		"block_budget":     blocks.MemoryBudget,
		"extent_budget":    extents.MemoryBudget,
		"buffers":          blocks.Buffers,
		"extent_caches":    extents.Caches,
	}
}
