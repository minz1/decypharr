package manager

import (
	"errors"

	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

func (m *Manager) RemoveFromProvider(providerEntry *storage.ProviderEntry) error {
	if providerEntry == nil {
		return nil
	}
	if providerEntry.Provider == usenetProvider {
		if m.usenet != nil {
			return m.usenet.Delete(providerEntry.ID)
		}
		return nil
	}

	client := m.ProviderClient(providerEntry.Provider)
	if client == nil {
		return nil
	}
	if err := client.DeleteTorrent(providerEntry.ID); err != nil {
		return err
	}
	if providerEntry.HoldsSlot() {
		m.slotFreed(providerEntry.Provider)
	}
	return nil
}

func (m *Manager) RemoveTorrentPlacements(t *storage.Entry) {
	for _, placement := range t.Providers {
		_ = m.RemoveFromProvider(placement)
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
	if removeErr := m.RemoveFromProvider(placement); removeErr != nil {
		m.logger.Warn().Err(removeErr).Str("name", entry.Name).Str("debrid", placement.Provider).
			Msg("Failed to delete the provider torrent of a removed download")
	}
}
