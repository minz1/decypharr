package manager

import (
	"context"
	"errors"
	"strings"

	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// askEntry sends msg to key's actor and returns the reducer's answer. A refusal means the grab is not in a phase
// that takes the event, and for a grab that is gone that is the answer the caller wants.
func (m *Manager) askEntry(ctx context.Context, key string, msg entryMsg) error {
	v, err := m.entries.runner.Ask(ctx, strings.ToLower(key), msg)
	if err == nil && v != nil {
		err, _ = v.(error)
	}
	if grab.IsRefused(err) {
		return nil
	}
	return err
}

// DeleteQueued deletes key's grab: its actor cancels the grab's children, then cleans up what cleanup asks for.
// A grab that is already gone or going is no error.
func (m *Manager) DeleteQueued(ctx context.Context, key string, cleanup grab.Cleanup) error {
	return m.askEntry(ctx, key, eventMsg(grab.Event{Kind: grab.EvDelete, Cleanup: cleanup}))
}

// AwaitGone returns once key has no grab: its record and files are removed.
func (m *Manager) AwaitGone(ctx context.Context, key string) error {
	select {
	case <-m.grabs.goneChan(key):
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// DeleteQueuedAll deletes each key's grab and returns every failure.
func (m *Manager) DeleteQueuedAll(ctx context.Context, keys []string, cleanup grab.Cleanup) error {
	var errs []error
	for _, key := range keys {
		errs = append(errs, m.DeleteQueued(ctx, key, cleanup))
	}
	return errors.Join(errs...)
}

// SetTags replaces key's tags; the grab's phase does not change.
func (m *Manager) SetTags(ctx context.Context, key string, tags []string) error {
	msg := eventMsg(grab.Event{Kind: grab.EvTagsChanged})
	msg.tags = tags
	return m.askEntry(ctx, key, msg)
}

// SetCategory sets key's category; the grab's phase does not change.
func (m *Manager) SetCategory(ctx context.Context, key, category string) error {
	msg := eventMsg(grab.Event{Kind: grab.EvCategoryChanged})
	msg.category = category
	return m.askEntry(ctx, key, msg)
}

// progressSink is how the action of key's grab reports its progress: a coalesced observation to the actor, which
// decides what is worth saving.
func (m *Manager) progressSink(key string) func(entry *storage.Entry) {
	return func(entry *storage.Entry) {
		progress, size, speed, at := entry.Progress, entry.SizeDownloaded, entry.Speed, entry.UpdatedAt
		msg := eventMsg(grab.Event{Kind: grab.EvActionProgress})
		msg.progress = progress
		msg.update = func(e *storage.Entry) {
			e.IsDownloading, e.SizeDownloaded, e.Speed = true, size, speed
			if !at.IsZero() {
				e.UpdatedAt = at
			}
		}
		if err := m.entries.runner.Send(strings.ToLower(key), msg); err != nil {
			m.logger.Debug().Err(err).Str("key", key).Msg("Download progress not delivered")
		}
	}
}

// completed runs what a grab that finished its action triggers outside its record.
func (m *Manager) completed(g grab.Grab) {
	m.strm.SyncEntryAsync(g.Entry)
	m.downloader.notifyCompleted(g.Entry)
	m.refreshArr(g.Entry)
	m.ReindexArrEntry(g.Entry.Category, g.Entry.InfoHash)
}

// DeleteTorrent deletes key's grab and its files; with removeFromDebrid it also removes the library entry and every
// provider placement of the torrent that no other entry holds.
func (m *Manager) DeleteTorrent(ctx context.Context, key string, removeFromDebrid bool) error {
	entry, err := m.queue.GetTorrent(key)
	if err != nil && !errors.Is(err, appendstore.ErrKeyNotFound) {
		return err
	}
	if err = m.DeleteQueued(ctx, key, grab.Cleanup{DeleteFiles: true}); err != nil || !removeFromDebrid {
		return err
	}
	if err = m.AwaitGone(ctx, key); err != nil {
		return err
	}
	if exists, _ := m.EntryExists(key); exists {
		return m.DeleteEntry(key, true)
	}
	if entry != nil {
		m.RemoveTorrentPlacements(entry)
	}
	return nil
}

// DeleteFailedNZBs deletes the grabs and files of the failed NZBs of category.
func (m *Manager) DeleteFailedNZBs(ctx context.Context, category string) error {
	failed, err := m.queue.ListFilter(category, config.ProtocolNZB, storage.EntryStateError, nil, "", false)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(failed))
	for _, e := range failed {
		keys = append(keys, e.InfoHash)
	}
	return m.DeleteQueuedAll(ctx, keys, grab.Cleanup{DeleteFiles: true})
}
