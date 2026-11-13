package manager

import (
	"context"
	"errors"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// placementFailure starts the log line for a failed provider deletion: WARN with failMsg, or INFO as left behind
// when the manager's stop refused or cut it, which is not a provider failure. It returns the event and its message.
func (m *Manager) placementFailure(err error, failMsg string) (*zerolog.Event, string) {
	if placementStopped(err) {
		return m.logger.Info().Err(err), "Provider torrent left behind (stopping)"
	}
	return m.logger.Warn().Err(err), failMsg
}

// RemoveFromProvider deletes one provider torrent for the entry self, unless another entry still holds it. ctx is
// the caller's ctx: the delete itself runs on the manager's stop grace, never on ctx.
func (m *Manager) RemoveFromProvider(ctx context.Context, providerEntry *storage.ProviderEntry, self string) error {
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
	_, err := m.releaseWait(ctx, placementRelease{
		provider: providerEntry.Provider, id: providerEntry.ID, grab: self, delete: true,
		slot: providerEntry.HoldsSlot(), library: true,
	})
	return err
}

// discardSubmitted deletes a torrent that was just added at a provider and given up on, for the entry self. A
// provider answers an add of a magnet it already holds with the existing torrent's ID, so the torrent may be one
// another entry holds; it is released, not deleted outright. The caller has accounted for any slot the add held.
func (m *Manager) discardSubmitted(ctx context.Context, provider, id, self string) error {
	if id == "" {
		return nil
	}
	err := m.releaseNoWait(ctx, placementRelease{provider: provider, id: id, grab: self, delete: true})
	if err != nil {
		ev, msg := m.placementFailure(err, "Failed to hand a provider torrent that was given up on to its holder")
		ev.Str("provider", provider).Str("id", id).Msg(msg)
	}
	return err
}

// releaseNoWait hands a release to the provider torrent's holder without waiting for the delete; it fails only when
// the holder refuses it. A usenet release has no holder: it runs inline and a failure is logged.
func (m *Manager) releaseNoWait(ctx context.Context, r placementRelease) error {
	if err := m.placementRefusal(ctx); r.delete && err != nil {
		return err
	}
	if r.provider != usenetProvider {
		return m.sendRelease(r, nil)
	}
	if _, err := m.releaseUsenet(ctx, r.id, r.grab); err != nil {
		ev, msg := m.placementFailure(err, "Failed to delete a usenet placement")
		ev.Str("id", r.id).Msg(msg)
	}
	return nil
}

// releaseAsync is releaseNoWait for a caller that only logs a refusal.
func (m *Manager) releaseAsync(ctx context.Context, name string, r placementRelease) {
	if err := m.releaseNoWait(ctx, r); err != nil {
		ev, msg := m.placementFailure(err, "Provider torrent not released; it may be left behind")
		ev.Str("name", name).Str("provider", r.provider).Str("id", r.id).Msg(msg)
	}
}

// startPlacementRun starts a provider deletion as a task of the placements group. The deletion can take minutes
// (the provider client retries) and runs on the stop grace, so it never runs inline in a caller that something else
// waits on and it survives the caller's stop for at most placementStopGrace. The group stops after the services that
// can start a deletion (repair, the reacquire worker, the mount, the API) and before usenet and storage, which a
// deletion in flight reads: Stop's deadline bounds the wait. run receives the placement-run task's ctx. It reports
// false, and runs nothing, once the group has closed; the refusal is logged with the entry and the provider, and the
// provider's torrent is found again by the next sync.
func (m *Manager) startPlacementRun(entry, provider string, run func(ctx context.Context)) bool {
	err := m.placementsSvc.group.Go("placement-run", func(ctx context.Context) error {
		run(ctx)
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
func (m *Manager) dropSourcePlacement(ctx context.Context, entry *storage.Entry, provider string) {
	entry.RemoveProvider(provider, m.debridOrder(), func(placement *storage.ProviderEntry) error {
		return m.RemoveFromProvider(ctx, placement, entry.InfoHash)
	})
}

// RemoveTorrentPlacements deletes the provider torrents of an entry that is gone, except those another entry holds.
// It runs inline and synchronous on the placements group ctx, never a request ctx, so a client disconnect does not
// undo a user's delete.
func (m *Manager) RemoveTorrentPlacements(t *storage.Entry) {
	ctx := m.placementsSvc.group.Context()
	for _, placement := range t.Providers {
		if err := m.RemoveFromProvider(ctx, placement, t.InfoHash); err != nil {
			ev, msg := m.placementFailure(err, "Failed to delete a provider torrent of a removed entry")
			ev.Str("infohash", t.InfoHash).Str("provider", placement.Provider).Msg(msg)
		}
	}
}

// stopProviderDownload deletes the provider torrent of an entry still downloading there, so a removed grab frees its slot.
func (m *Manager) stopProviderDownload(ctx context.Context, entry *storage.Entry) error {
	placement := entry.GetActiveProvider()
	if !entry.IsTorrent() || entry.IsComplete || placement == nil || !placement.HoldsSlot() {
		return nil
	}
	stored, err := m.storage.Get(entry.InfoHash)
	switch {
	case errors.Is(err, appendstore.ErrKeyNotFound):
	case err != nil:
		m.logger.Warn().Err(err).Str("name", entry.Name).Msg("Kept the provider torrent of a removed download")
		return nil
	default:
		if lib := stored.Providers[placement.Provider]; lib != nil && lib.ID == placement.ID {
			return nil
		}
	}
	err = m.releaseNoWait(ctx, placementRelease{
		provider: placement.Provider, id: placement.ID, grab: entry.InfoHash, delete: true, slot: placement.HoldsSlot(),
	})
	if err != nil {
		ev, msg := m.placementFailure(err, "Failed to hand the provider torrent of a removed download to its holder")
		ev.Str("name", entry.Name).Str("debrid", placement.Provider).Msg(msg)
	}
	return err
}
