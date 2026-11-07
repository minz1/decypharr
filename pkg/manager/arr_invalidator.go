package manager

import (
	"context"
	"errors"
	"fmt"

	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
)

// InvalidateReacquire removes the managed entries a reacquire job replaces, once the job has deleted their Arr files
// and searched.
//
// Invariant: the Arr files a job deletes are exactly the imported files of the managed entries it invalidates. The
// job's bindings are the ones that authorize a mutation (see reacquire's enqueue), and an entry is invalidated whole,
// so CheckInvalidation refuses an entry that has an imported file the job does not delete: its Arr file would
// survive the deletion of its data.
func (m *Manager) InvalidateReacquire(ctx context.Context, job reacquire.Job) error {
	if job.Strategy == reacquire.StrategyDownloadFailed {
		return m.removeFailedDownloadRecord(job.EntryID)
	}
	if err := m.CheckInvalidation(ctx, job); err != nil {
		return err
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

// CheckInvalidation fails with reacquire.OperatorRequired when invalidating the job's entries would leave an Arr file
// the job does not delete behind: every file of an invalidated entry that the Arr imported must be bound in the job
// by a binding that authorizes the mutation. It changes nothing, so a job can ask before it touches the Arr.
func (m *Manager) CheckInvalidation(_ context.Context, job reacquire.Job) error {
	if job.Strategy == reacquire.StrategyDownloadFailed || m.arrService == nil {
		return nil
	}
	deleted := make(map[[2]string]struct{}, len(job.Bindings))
	for _, binding := range job.Bindings {
		if binding.AuthorizesMutation() {
			deleted[[2]string{binding.EntryID, binding.EntryFileID}] = struct{}{}
		}
	}
	for entryID := range invalidatedEntryIDs(job) {
		entry, err := m.storage.Get(entryID)
		if errors.Is(err, appendstore.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read managed entry %q: %w", entryID, err)
		}
		for _, file := range entry.Files {
			binding, imported := m.arrService.Lookup(entryID, file.ID)
			if !imported || binding.ArrFileID <= 0 {
				continue
			}
			if _, ok := deleted[[2]string{entryID, file.ID}]; !ok {
				return reacquire.OperatorRequired(fmt.Sprintf(
					"%s of %q is imported as Arr file %d but its binding does not authorize deleting it, "+
						"so the entry was kept; delete that Arr file or fix its binding, then acknowledge the job",
					file.Name, entry.Name, binding.ArrFileID,
				))
			}
		}
	}
	return nil
}

// removeFailedDownloadRecord drops the queue record of a download whose grab the Arr failed, as a client delete would.
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
