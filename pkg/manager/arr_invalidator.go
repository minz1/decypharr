package manager

import (
	"context"
	"fmt"

	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
)

func (m *Manager) InvalidateReacquire(ctx context.Context, job reacquire.Job) error {
	if job.Strategy == reacquire.StrategyDownloadFailed {
		return m.removeFailedDownloadRecord(job.EntryID)
	}
	for entryID := range invalidatedEntryIDs(job) {
		if err := ctx.Err(); err != nil {
			return err
		}
		exists, err := m.storage.Exists(entryID)
		if err != nil {
			return fmt.Errorf("check managed entry %q: %w", entryID, err)
		}
		if !exists {
			continue
		}
		if deleteEntryErr := m.DeleteEntry(entryID, true); deleteEntryErr != nil {
			return fmt.Errorf("invalidate managed entry %q: %w", entryID, deleteEntryErr)
		}
	}
	if m.arrService != nil {
		for _, binding := range job.Bindings {
			if err := m.arrService.DeleteBinding(binding.EntryID, binding.EntryFileID); err != nil {
				return fmt.Errorf("remove Arr binding %q/%q: %w", binding.EntryID, binding.EntryFileID, err)
			}
		}
	}
	return nil
}

// removeFailedDownloadRecord drops the queue record of a download whose grab
// the Arr has failed. The entry is already gone; the record is all the Arr
// still tracks, and removing it is what a client-side delete would do.
func (m *Manager) removeFailedDownloadRecord(entryID string) error {
	if entryID == "" || !m.queue.Contains(entryID) {
		return nil
	}
	return m.queue.Delete(entryID, true, nil)
}

// invalidatedEntryIDs is the set of managed entries a reacquire job replaces:
// its bindings' entries, or the job's own entry when it has no bindings.
func invalidatedEntryIDs(job reacquire.Job) map[string]struct{} {
	entryIDs := make(map[string]struct{}, len(job.Bindings))
	for _, binding := range job.Bindings {
		if binding.EntryID != "" {
			entryIDs[binding.EntryID] = struct{}{}
		}
	}
	if len(entryIDs) == 0 && job.EntryID != "" {
		entryIDs[job.EntryID] = struct{}{}
	}
	return entryIDs
}
