package manager

import (
	"context"
	"errors"

	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// RemoveFromProvider deletes one provider torrent for the entry self, unless another entry still holds it.
func (m *Manager) RemoveFromProvider(providerEntry *storage.ProviderEntry, self string) error {
	if providerEntry == nil || providerEntry.ID == "" {
		return nil
	}
	if providerEntry.Provider == usenetProvider {
		if m.usenet == nil && m.removeUsenet == nil {
			return nil
		}
	} else if m.ProviderClient(providerEntry.Provider) == nil {
		return nil
	}
	_, err := m.releasePlacement(providerEntry.Provider, providerEntry, self)
	return err
}

// discardSubmitted deletes a torrent that was just added at a provider and given up on, for the entry self. A
// provider answers an add of a magnet it already holds with the existing torrent's ID, so the torrent may be one
// another entry holds; it is released, not deleted outright. The caller has accounted for any slot the add held.
func (m *Manager) discardSubmitted(provider, id, self string) {
	if id == "" {
		return
	}
	placement := &storage.ProviderEntry{Provider: provider, ID: id, Progress: 1}
	if _, err := m.releasePlacement(provider, placement, self); err != nil {
		m.logger.Warn().Err(err).Str("provider", provider).Str("id", id).
			Msg("Failed to delete a provider torrent that was given up on")
	}
}

// startPlacementRun starts a provider deletion as a task of the placements group. The deletion can take minutes
// (the provider client retries) and ignores the group's context, so it never runs inline in a caller that
// something else waits on. The group stops after the services that can start a deletion (repair, the reacquire
// worker, the mount, the API) and before usenet and storage, which a deletion in flight reads: Stop's deadline
// bounds the wait. It reports false, and runs nothing, once the group has closed; the refusal is logged with the
// entry and the provider, and the provider's torrent is found again by the next sync.
func (m *Manager) startPlacementRun(entry, provider string, run func()) bool {
	err := m.placementsSvc.group.Go("placement-run", func(context.Context) error {
		run()
		return nil
	})
	if err != nil {
		m.logger.Warn().Err(err).Str("name", entry).Str("provider", provider).
			Msg("Manager is stopping; a provider torrent was not deleted and may be left behind")
		return false
	}
	return true
}

// dropSourcePlacement takes the placements an entry has on provider away from it and deletes them there.
func (m *Manager) dropSourcePlacement(entry *storage.Entry, provider string) {
	entry.RemoveProvider(provider, func(placement *storage.ProviderEntry) error {
		return m.RemoveFromProvider(placement, entry.InfoHash)
	})
}

// RemoveTorrentPlacements deletes the provider torrents of an entry that is gone, except those another entry holds.
func (m *Manager) RemoveTorrentPlacements(t *storage.Entry) {
	for _, placement := range t.Providers {
		if err := m.RemoveFromProvider(placement, t.InfoHash); err != nil {
			m.logger.Warn().Err(err).Str("infohash", t.InfoHash).Str("provider", placement.Provider).
				Msg("Failed to delete a provider torrent of a removed entry")
		}
	}
}

// RemoveQueued deletes a queued entry, then its provider download: a poll that sees the provider torrent gone must not find the entry.
func (m *Manager) RemoveQueued(infohash string, deleteFiles bool) error {
	entry, err := m.queue.GetTorrent(infohash)
	if errors.Is(err, appendstore.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if deleteErr := m.queue.Delete(infohash, deleteFiles, nil); deleteErr != nil {
		return deleteErr
	}
	m.stopProviderDownload(entry)
	return nil
}

// stopProviderDownload deletes the provider torrent of an entry still downloading there, so a removed grab frees its slot.
func (m *Manager) stopProviderDownload(entry *storage.Entry) {
	placement := entry.GetActiveProvider()
	if !entry.IsTorrent() || entry.IsComplete || placement == nil || !placement.HoldsSlot() {
		return
	}
	stored, err := m.storage.Get(entry.InfoHash)
	switch {
	case errors.Is(err, appendstore.ErrKeyNotFound):
	case err != nil:
		m.logger.Warn().Err(err).Str("name", entry.Name).Msg("Kept the provider torrent of a removed download")
		return
	default:
		if lib := stored.Providers[placement.Provider]; lib != nil && lib.ID == placement.ID {
			return
		}
	}
	if removeErr := m.RemoveFromProvider(placement, entry.InfoHash); removeErr != nil {
		m.logger.Warn().Err(removeErr).Str("name", entry.Name).Str("debrid", placement.Provider).
			Msg("Failed to delete the provider torrent of a removed download")
	}
}
