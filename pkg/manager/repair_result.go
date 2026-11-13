package manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type repairOutcome uint8

const (
	repairHealed repairOutcome = iota
	repairBad
)

// RepairResult is what a repair decided, applied by one merge onto a fresh read of the entry.
type RepairResult struct {
	Outcome  repairOutcome
	Provider string
	// Placement is the fresh submit the repair made active; nil when an existing placement was activated.
	Placement *types.Torrent
	// SeenID is the ID of the placement the repair saw on Provider, if any.
	SeenID string
	// Release is the old placement the repair replaced on the same provider; it is released once the merge lands.
	Release *placementRelease
}

var (
	errActivateFailed   = errors.New("placement could not be activated")
	errPlacementChanged = errors.New("placement changed during the repair")
	errRepairSuperseded = errors.New("entry was removed during the repair")
)

// applyRepair merges r onto fresh and returns the merged copy; fresh is left alone. ErrNoChange means nothing
// changes.
func applyRepair(fresh *storage.Entry, r RepairResult, now time.Time) (*storage.Entry, error) {
	if fresh == nil {
		return nil, storage.ErrNoChange
	}
	if r.Outcome == repairBad {
		return withBad(fresh, true, now)
	}
	out := fresh.Clone()
	if r.Placement != nil {
		if cur := out.Providers[r.Provider]; cur != nil && cur.ID != "" && cur.ID != r.Placement.ID && cur.ID != r.SeenID {
			return nil, errPlacementChanged
		}
		adoptPlacement(out, r.Placement, now)
	}
	if err := out.ActivatePlacement(r.Provider); err != nil {
		return nil, fmt.Errorf("%w: %w", errActivateFailed, err)
	}
	out.Bad = false
	out.UpdatedAt = now
	return out, nil
}

// withBad returns a copy of fresh with its bad flag set to bad; ErrNoChange when it already is.
func withBad(fresh *storage.Entry, bad bool, now time.Time) (*storage.Entry, error) {
	if fresh.Bad == bad {
		return nil, storage.ErrNoChange
	}
	out := fresh.Clone()
	out.Bad = bad
	out.UpdatedAt = now
	return out, nil
}

// adoptPlacement records a new placement on entry and revives its files.
func adoptPlacement(entry *storage.Entry, placement *types.Torrent, now time.Time) {
	addedOn := placement.Added
	if addedOn.IsZero() {
		addedOn = now
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
			InfoHash:  entry.InfoHash,
			AddedOn:   addedOn,
		}
	}
}

// applyRepairResult merges r onto key's stored entry (through the actor when the key has a grab). Once the merge
// landed it releases the placement the repair replaced; when it did not, the placement the repair submitted is
// discarded and the old one stays.
func (m *Manager) applyRepairResult(ctx context.Context, key string, r RepairResult) error {
	var merged *storage.Entry
	var gone, flipped bool
	err := m.mergeLibrary(ctx, key, func(fresh *storage.Entry) (*storage.Entry, error) {
		if fresh == nil {
			gone = true
			return nil, storage.ErrNoChange
		}
		out, mergeErr := applyRepair(fresh, r, time.Now())
		if mergeErr == nil {
			merged, flipped = out, fresh.Bad != out.Bad
		}
		return out, mergeErr
	})
	switch {
	case gone:
		err = errRepairSuperseded
	case errors.Is(err, errActivateFailed), errors.Is(err, errPlacementChanged):
	case err != nil:
		return err
	case merged == nil:
		return nil
	}
	work := m.work.Context()
	if err != nil {
		if r.Placement != nil {
			_ = m.discardSubmitted(work, r.Provider, r.Placement.ID, key)
		}
		return err
	}
	if r.Release != nil {
		m.releaseAsync(work, merged.Name, *r.Release)
	}
	if merged.IsComplete {
		m.strm.SyncEntryAsync(merged)
	}
	if flipped {
		m.refreshLater()
	}
	return nil
}
