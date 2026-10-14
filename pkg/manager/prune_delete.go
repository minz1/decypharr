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

type placementKey struct{ provider, id string }

type pruneOutcome int

const (
	pruneDeleted pruneOutcome = iota
	pruneSkipped
	pruneFailed
	pruneStorageFailed
)

type pruneState struct {
	holders     map[placementKey]map[string]struct{}
	reacquiring map[string]struct{}
}

func (m *Manager) pruneEntries(ctx context.Context, eligible []PruneCandidate) (int, int, int64) {
	var deleted, failed int
	var freed int64
	st, err := m.newPruneState()
	if err != nil {
		m.logger.Error().Err(err).Msg("prune delete skipped: cannot establish placement holders")
		return 0, len(eligible), 0
	}
	for _, c := range eligible {
		if ctx.Err() != nil {
			break
		}
		outcome, size := m.pruneOne(c, st)
		switch outcome {
		case pruneDeleted:
			deleted++
			freed += size
		case pruneFailed:
			failed++
		case pruneStorageFailed:
			failed++
			return m.finishPruneBatch(deleted), failed, freed
		case pruneSkipped:
		}
	}
	return m.finishPruneBatch(deleted), failed, freed
}

func (m *Manager) finishPruneBatch(deleted int) int {
	if deleted == 0 {
		return 0
	}
	m.InvalidateEntryCache()
	if err := m.RefreshMount(); err != nil {
		m.logger.Error().Err(err).Msg("Mount refresh after prune failed")
	}
	return deleted
}

func (m *Manager) newPruneState() (*pruneState, error) {
	reacquiring, err := m.reacquiringEntries()
	if err != nil {
		return nil, err
	}
	holders := map[placementKey]map[string]struct{}{}
	err = m.storage.ForEachStrict(func(e *storage.Entry) error {
		for name, p := range e.Providers {
			if p == nil || p.ID == "" {
				continue
			}
			key := placementKey{name, p.ID}
			if holders[key] == nil {
				holders[key] = map[string]struct{}{}
			}
			holders[key][e.InfoHash] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	return &pruneState{holders: holders, reacquiring: reacquiring}, nil
}

func (m *Manager) pruneOne(c PruneCandidate, st *pruneState) (pruneOutcome, int64) {
	log := m.logger.With().Str("infohash", c.InfoHash).Logger()
	e, err := m.storage.Get(c.InfoHash)
	if errors.Is(err, appendstore.ErrKeyNotFound) {
		if clearErr := m.storage.ClearUnreferencedSince(c.InfoHash); clearErr != nil {
			log.Warn().Err(clearErr).Msg("prune: clear ledger key of vanished entry failed")
		}
		return pruneSkipped, 0
	}
	if err != nil {
		log.Warn().Err(err).Msg("prune: read entry failed, keeping it")
		return pruneFailed, 0
	}
	if reason := m.pruneExclusion(e, st.reacquiring); reason != "" {
		return pruneSkipped, 0
	}
	if err = m.pruneDeleteBindings(e); err != nil {
		log.Warn().Err(err).Msg("prune: delete arr binding failed, keeping entry")
		return pruneFailed, 0
	}
	if err = m.pruneRemovePlacements(e, st); err != nil {
		log.Warn().Err(err).Msg("prune: provider removal failed, keeping entry")
		return pruneFailed, 0
	}
	if err = m.storage.Delete(e.InfoHash); err != nil {
		log.Error().Err(err).Msg("prune: providers removed but storage delete failed, stopping batch")
		return pruneStorageFailed, 0
	}
	if m.strm != nil {
		m.strm.RemoveEntryAsync(e)
	}
	if err = m.storage.ClearUnreferencedSince(e.InfoHash); err != nil {
		log.Warn().Err(err).Msg("prune: clear ledger key failed")
	}
	for name, p := range e.Providers {
		if p != nil {
			delete(st.holders[placementKey{name, p.ID}], e.InfoHash)
		}
	}
	log.Info().
		Str("name", e.Name).
		Str("provider", c.Provider).
		Int64("size", e.Size).
		Int("unreferenced_days", int(m.clockNow().Sub(c.UnreferencedSince)/pruneDay)).
		Msg("prune deleted entry")
	return pruneDeleted, e.Size
}

func (m *Manager) pruneDeleteBindings(e *storage.Entry) error {
	recovery := m.recoveryService()
	if recovery == nil {
		return nil
	}
	for _, f := range e.Files {
		if _, ok := recovery.Lookup(e.InfoHash, f.ID); !ok {
			continue
		}
		if err := recovery.DeleteBinding(e.InfoHash, f.ID); err != nil {
			return fmt.Errorf("file %s: %w", f.ID, err)
		}
	}
	return nil
}

func (m *Manager) pruneRemovePlacements(e *storage.Entry, st *pruneState) error {
	for name, p := range e.Providers {
		if p == nil || p.ID == "" {
			continue
		}
		holders := st.holders[placementKey{name, p.ID}]
		if _, self := holders[e.InfoHash]; !self {
			return fmt.Errorf("placement %s/%s is not in the holder snapshot", name, p.ID)
		}
		if len(holders) > 1 {
			continue
		}
		if err := m.removePlacement(name, p.ID); err != nil {
			return fmt.Errorf("%s/%s: %w", name, p.ID, err)
		}
	}
	return nil
}

func (m *Manager) removePlacement(provider, id string) error {
	lock := m.providerLock(provider)
	lock.Lock()
	defer lock.Unlock()
	if provider == usenetProvider {
		return m.removeUsenetPlacement(id)
	}
	client := m.ProviderClient(provider)
	if client == nil {
		return errors.New("provider not configured")
	}
	delErr := client.DeleteTorrent(id)
	if delErr == nil {
		return nil
	}
	if _, getErr := client.GetTorrent(id); errors.Is(getErr, customerror.ErrTorrentNotFound) {
		return nil
	}
	return delErr
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
