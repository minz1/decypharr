package manager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const pruneDay = 24 * time.Hour

var errPlacementHeld = errors.New("placement held by another entry")

type pruneOutcome int

const (
	pruneDeleted pruneOutcome = iota
	pruneSkipped
	pruneFailed
	pruneStorageFailed
)

func (m *Manager) recordPruneOutcome(hash string, outcome pruneOutcome) {
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
		outcome, size := m.pruneOne(c)
		m.recordPruneOutcome(c.InfoHash, outcome)
		switch outcome {
		case pruneDeleted:
			deleted++
			freed += size
		case pruneFailed:
			failed++
		case pruneStorageFailed:
			return deleted, failed + 1, freed
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

func (m *Manager) pruneOne(c PruneCandidate) (pruneOutcome, int64) {
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
	removed, err := m.pruneRemovePlacements(e)
	if err != nil {
		log.Warn().Err(err).Msg("prune: provider removal failed, keeping entry")
		return pruneFailed, 0
	}
	if err = m.storage.Delete(e.InfoHash); errors.Is(err, appendstore.ErrKeyNotFound) {
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

func (m *Manager) pruneRemovePlacements(e *storage.Entry) (bool, error) {
	removed := false
	for name, p := range e.Providers {
		if p == nil || p.ID == "" {
			continue
		}
		ran, err := m.removePlacement(name, p.ID, e.InfoHash)
		if err != nil {
			return removed, fmt.Errorf("%s/%s: %w", name, p.ID, err)
		}
		if ran && p.HoldsSlot() {
			m.slotFreed(name)
		}
		removed = removed || ran
	}
	return removed, nil
}

func (m *Manager) placementHeldElsewhere(provider, id, self string) (bool, error) {
	err := m.storage.ForEachStrict(func(e *storage.Entry) error {
		if p := e.Providers[provider]; e.InfoHash != self && p != nil && p.ID == id {
			return errPlacementHeld
		}
		return nil
	})
	if errors.Is(err, errPlacementHeld) {
		return true, nil
	}
	return false, err
}

func (m *Manager) removePlacement(provider, id, self string) (bool, error) {
	lock := m.providerLock(provider)
	lock.Lock()
	defer lock.Unlock()
	held, err := m.placementHeldElsewhere(provider, id, self)
	if err != nil {
		return false, fmt.Errorf("recount holders: %w", err)
	}
	if held {
		return false, nil
	}
	if provider == usenetProvider {
		return true, m.removeUsenetPlacement(id)
	}
	client := m.ProviderClient(provider)
	if client == nil {
		return false, errors.New("provider not configured")
	}
	delErr := client.DeleteTorrent(id)
	if delErr == nil {
		return true, nil
	}
	if _, getErr := client.GetTorrent(id); errors.Is(getErr, customerror.ErrTorrentNotFound) {
		return true, nil
	}
	return false, delErr
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
