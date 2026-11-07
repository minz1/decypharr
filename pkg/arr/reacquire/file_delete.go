package reacquire

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sirrobot01/decypharr/pkg/arr"
)

// FileDelete journals the delete of one Arr file. The Arr unmonitors an
// episode whose file is deleted (when it is set to), so the episodes that were
// monitored before the DELETE are recorded with it: a delete that may have
// landed is settled by finding the file gone, and the episodes put back.
//
// It is a Job field of its own, not a Mutation: the strict mutation kinds
// would make an older binary refuse the whole job. An older binary ignores the
// field, and drops it when it saves the job again.
type FileDelete struct {
	ArrFileID int `json:"arrFileId"`
	// EpisodeIDs are the episodes of the file that were monitored before the first DELETE.
	EpisodeIDs []int `json:"episodeIds,omitempty"`
	// AttemptedAt is when the first DELETE was recorded, before it was sent.
	AttemptedAt time.Time `json:"attemptedAt"`
	// Attempts counts the DELETEs sent.
	Attempts int `json:"attempts,omitzero"`
	// ConfirmedAt is when the file was known gone: the DELETE answered, or the file was not found.
	ConfirmedAt time.Time `json:"confirmedAt,omitzero"`
	// Remonitored is set once EpisodeIDs are monitored again.
	Remonitored bool `json:"remonitored,omitempty"`
}

func cloneFileDelete(record FileDelete) FileDelete {
	record.EpisodeIDs = slices.Clone(record.EpisodeIDs)
	return record
}

// unconfirmed reports a delete that was recorded and may have been sent, but
// that nothing has shown landed. It counts like an attempted mutation intent:
// the job must neither fail nor be forgotten while one is open.
func (record FileDelete) unconfirmed() bool {
	return record.ConfirmedAt.IsZero()
}

// remonitorOwed reports a landed delete whose episodes are not monitored again.
func (record FileDelete) remonitorOwed() bool {
	return !record.ConfirmedAt.IsZero() && len(record.EpisodeIDs) > 0 && !record.Remonitored
}

// hasAttemptedIntent reports a remote action that may have landed and that
// nothing has confirmed: a dispatched mutation or an unconfirmed file delete.
// The job is left to an operator rather than failed.
func (job Job) hasAttemptedIntent() bool {
	return slices.ContainsFunc(job.Mutations, func(mutation Mutation) bool {
		return mutation.State == MutationIntent && mutation.Attempts > 0
	}) || slices.ContainsFunc(job.FileDeletes, FileDelete.unconfirmed)
}

func fileDeleteFor(job Job, arrFileID int) (FileDelete, bool) {
	for _, record := range job.FileDeletes {
		if record.ArrFileID == arrFileID {
			return cloneFileDelete(record), true
		}
	}
	return FileDelete{}, false
}

func setFileDelete(job *Job, record FileDelete) {
	for i := range job.FileDeletes {
		if job.FileDeletes[i].ArrFileID == record.ArrFileID {
			job.FileDeletes[i] = cloneFileDelete(record)
			return
		}
	}
	job.FileDeletes = append(job.FileDeletes, cloneFileDelete(record))
}

// persistFileDelete stores the record durably, then in the handler's copy of the job.
func persistFileDelete(job *Job, progress JobProgress, record FileDelete) error {
	if err := progress.UpdateDurable(StatusInvalidating, func(current *Job) {
		setFileDelete(current, record)
	}); err != nil {
		return err
	}
	setFileDelete(job, record)
	return nil
}

// deleteArrFile journals the delete of one present file, then sends it. The
// record is durable before the DELETE leaves, so a crash or a lost answer
// leaves evidence that it may have landed.
func (handler *arrHandler) deleteArrFile(
	ctx context.Context,
	instance arr.Arr,
	job *Job,
	progress JobProgress,
	arrFileID int,
) error {
	record, journaled := fileDeleteFor(*job, arrFileID)
	if journaled && record.Attempts >= maxMutationAttempts {
		return fmt.Errorf(
			"arr file %d was deleted %d times and is still listed; refusing another automatic delete",
			arrFileID, record.Attempts,
		)
	}
	if !journaled {
		episodes, err := handler.arrs.MonitoredEpisodesOfFile(ctx, instance.Name, arrFileID)
		if err != nil {
			return fmt.Errorf("%w: %w", errArrLookup, err)
		}
		record = FileDelete{ArrFileID: arrFileID, EpisodeIDs: episodes, AttemptedAt: time.Now().UTC()}
	}
	record.Attempts++
	if err := persistFileDelete(job, progress, record); err != nil {
		return err
	}
	if err := handler.arrs.DeleteLibraryFile(ctx, instance.Name, arrFileID); err != nil {
		return err
	}
	return handler.settleFileDelete(ctx, instance, job, progress, record)
}

// settleFileDelete records that the file is gone and puts the recorded
// episodes back to monitored. Monitoring is idempotent, so an attempt that was
// cut short is simply repeated. A refusal is not fatal: the file is already
// gone and the search still has to go out, and the record keeps the debt.
func (handler *arrHandler) settleFileDelete(
	ctx context.Context,
	instance arr.Arr,
	job *Job,
	progress JobProgress,
	record FileDelete,
) error {
	if record.ConfirmedAt.IsZero() {
		record.ConfirmedAt = time.Now().UTC()
		if err := persistFileDelete(job, progress, record); err != nil {
			return arr.UnknownMutationOutcome(fmt.Errorf("persist confirmed Arr file delete: %w", err), 0)
		}
	}
	if !record.remonitorOwed() {
		return nil
	}
	if monitorErr := handler.arrs.MonitorEpisodes(ctx, instance.Name, record.EpisodeIDs); monitorErr == nil {
		record.Remonitored = true
		if err := persistFileDelete(job, progress, record); err != nil {
			return arr.UnknownMutationOutcome(fmt.Errorf("persist re-monitored Arr episodes: %w", err), 0)
		}
	}
	return nil
}
