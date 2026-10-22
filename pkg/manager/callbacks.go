package manager

import (
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
	if providerEntry.Progress < 1 {
		m.slotFreed(providerEntry.Provider)
	}
	return nil
}

func (m *Manager) RemoveTorrentPlacements(t *storage.Entry) {
	for _, placement := range t.Providers {
		_ = m.RemoveFromProvider(placement)
	}
}
