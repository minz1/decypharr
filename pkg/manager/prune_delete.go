package manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const pruneDay = 24 * time.Hour

var errPlacementHeld = errors.New("placement held by another entry")

// errPlacementStopped marks a provider deletion the manager's stop refused or cut: the torrent is left at the
// provider, which is not a provider failure. Callers log it at INFO as "left behind (stopping)".
var errPlacementStopped = errors.New("provider deletion stopped")

// placementStopped reports whether err is a deletion the stop refused or cut, not a provider failure.
func placementStopped(err error) bool { return errors.Is(err, errPlacementStopped) }

type pruneOutcome int

const (
	pruneDeleted pruneOutcome = iota
	pruneSkipped
	pruneFailed
	pruneStorageFailed
	// pruneStopped: the stop refused or cut a provider removal; the entry is kept and the batch ends quietly.
	pruneStopped
)

func (m *Manager) recordPruneOutcome(hash string, outcome pruneOutcome) {
	if outcome == pruneStopped {
		return
	}
	if outcome != pruneFailed && outcome != pruneStorageFailed {
		delete(m.pruneFails, hash)
		return
	}
	if m.pruneFails == nil {
		m.pruneFails = map[string]int{}
	}
	m.pruneFails[hash]++
}

func (m *Manager) pruneEntries(ctx context.Context, eligible []PruneCandidate) (int, int, int64) {
	var deleted, failed int
	var freed int64
	defer func() { m.finishPruneBatch(deleted) }()
	for _, c := range eligible {
		if ctx.Err() != nil {
			break
		}
		outcome, size := m.pruneOne(ctx, c)
		m.recordPruneOutcome(c.InfoHash, outcome)
		switch outcome {
		case pruneDeleted:
			deleted++
			freed += size
		case pruneFailed:
			failed++
		case pruneStorageFailed:
			return deleted, failed + 1, freed
		case pruneStopped:
			return deleted, failed, freed
		case pruneSkipped:
		}
	}
	return deleted, failed, freed
}

func (m *Manager) finishPruneBatch(deleted int) {
	if deleted == 0 {
		return
	}
	m.InvalidateEntryCache()
	if err := m.RefreshMount(); err != nil {
		m.logger.Error().Err(err).Msg("Mount refresh after prune failed")
	}
}

func (m *Manager) pruneOne(ctx context.Context, c PruneCandidate) (pruneOutcome, int64) {
	log := m.logger.With().Str("infohash", c.InfoHash).Logger()
	e, err := m.storage.Get(c.InfoHash)
	if errors.Is(err, appendstore.ErrKeyNotFound) {
		m.clearPruneKey(c.InfoHash)
		return pruneSkipped, 0
	}
	if err != nil {
		log.Warn().Err(err).Msg("prune: read entry failed, keeping it")
		return pruneFailed, 0
	}
	reacquiring, err := m.reacquiringEntries()
	if err != nil {
		log.Warn().Err(err).Msg("prune: cannot check reacquire jobs, keeping entry")
		return pruneFailed, 0
	}
	if reason := m.pruneExclusion(e, reacquiring); reason != "" {
		return pruneSkipped, 0
	}
	removed, err := m.pruneRemovePlacements(ctx, e)
	if placementStopped(err) {
		log.Info().Err(err).Msg("prune: provider torrent left behind (stopping), keeping entry")
		return pruneStopped, 0
	}
	if err != nil {
		log.Warn().Err(err).Msg("prune: provider removal failed, keeping entry")
		return pruneFailed, 0
	}
	if err = m.pruneDelete(e.InfoHash); errors.Is(err, errPruneGrabbed) {
		return pruneSkipped, 0
	} else if errors.Is(err, appendstore.ErrKeyNotFound) {
		m.clearPruneKey(c.InfoHash)
		return pruneSkipped, 0
	} else if err != nil {
		log.Error().Err(err).Msg("prune: providers removed but storage delete failed, stopping batch")
		return pruneStorageFailed, 0
	}
	if m.strm != nil {
		m.strm.RemoveEntryAsync(e)
	}
	m.clearPruneKey(c.InfoHash)
	log.Info().
		Str("name", e.Name).
		Str("provider", c.Provider).
		Int64("size", e.Size).
		Int("unreferenced_days", int(m.clockNow().Sub(c.UnreferencedSince)/pruneDay)).
		Msg("prune deleted entry")
	if !removed {
		return pruneDeleted, 0
	}
	return pruneDeleted, e.Size
}

func (m *Manager) clearPruneKey(hash string) {
	if err := m.storage.ClearUnreferencedSince(hash); err != nil {
		m.logger.Warn().Err(err).Str("infohash", hash).Msg("prune: clear ledger key failed")
	}
}

func (m *Manager) pruneRemovePlacements(ctx context.Context, e *storage.Entry) (bool, error) {
	removed := false
	for name, p := range e.Providers {
		if p == nil || p.ID == "" {
			continue
		}
		ran, err := m.releaseWait(ctx, placementRelease{
			provider: name, id: p.ID, grab: e.InfoHash, delete: true, slot: p.HoldsSlot(), library: true,
		})
		if err != nil {
			return removed, fmt.Errorf("%s/%s: %w", name, p.ID, err)
		}
		removed = removed || ran
	}
	return removed, nil
}

// placementHeldElsewhere reports whether an entry other than self, stored or queued, lists the provider torrent id.
// Multi-season entries share the torrent of the pack they came from, so a torrent has as many holders as entries
// list it.
func (m *Manager) placementHeldElsewhere(provider, id, self string) (bool, error) {
	if m.storage == nil {
		return false, nil
	}
	holds := func(e *storage.Entry) bool {
		p := e.Providers[provider]
		return e.InfoHash != self && p != nil && p.ID == id
	}
	err := m.storage.ForEachStrict(func(e *storage.Entry) error {
		if holds(e) {
			return errPlacementHeld
		}
		return nil
	})
	if errors.Is(err, errPlacementHeld) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	queued, err := m.storage.FilterQueued(holds)
	return len(queued) > 0, err
}

// releaseUsenet deletes one usenet placement, unless an entry other than self still holds it. A usenet placement has
// no provider torrent, so it stays outside the holder; the provider's lock serialises it with a submission. It
// reports whether the placement was deleted.
func (m *Manager) releaseUsenet(ctx context.Context, id, self string) (bool, error) {
	lock := m.providerLock(usenetProvider)
	lock.Lock()
	defer lock.Unlock()
	if err := m.placementRefusal(ctx); err != nil {
		return false, err
	}
	held, err := m.placementHeldElsewhere(usenetProvider, id, self)
	if err != nil {
		return false, fmt.Errorf("recount holders: %w", err)
	}
	if held {
		return false, nil
	}
	return true, m.removeUsenetPlacement(id)
}

// deleteContext is the ctx a started provider delete runs on: the manager's stop grace. A hand-built test manager
// that never called initGrace has none; its delete then keeps ctx's values without its cancel, since a started
// delete must not end with its caller.
func (m *Manager) deleteContext(ctx context.Context) context.Context {
	if m.grace != nil {
		return m.grace
	}
	return context.WithoutCancel(ctx)
}

// placementRefusal reports why a provider deletion must not run: its owner ctx
// ended or the stop grace ran out. It reports nil while both are live, so a
// delete asked for after the stop began still gets what is left of the shared
// grace. A refusal wraps errPlacementStopped and the cause of the ctx that
// ended, and always names task.ErrStopped when the manager has begun stopping,
// so a release asked for after closeStorage released the grace still reads as
// a stop.
func (m *Manager) placementRefusal(ctx context.Context) error {
	var ended context.Context
	switch {
	case ctx.Err() != nil:
		ended = ctx
	case m.grace != nil && m.grace.Err() != nil:
		ended = m.grace
	default:
		return nil
	}
	err := fmt.Errorf("%w: %w", errPlacementStopped, context.Cause(ended))
	if m.stopErr() != nil && !errors.Is(err, task.ErrStopped) {
		err = fmt.Errorf("%w: %w", err, task.ErrStopped)
	}
	return err
}

func (m *Manager) removeUsenetPlacement(id string) error {
	del := m.removeUsenet
	if del == nil {
		if m.usenet == nil {
			return errors.New("usenet not configured")
		}
		del = m.usenet.Delete
	}
	if err := del(id); err != nil && !errors.Is(err, customerror.ErrUsenetManifestMissing) {
		return err
	}
	return nil
}
