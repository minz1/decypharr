package reacquire

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// searchCompensator is a Handler that can send the search a job owes after it
// deleted an Arr file and nothing else will look for the media.
type searchCompensator interface {
	CompensateSearch(context.Context, Job, JobProgress) error
}

func (job Job) hasFileDelete() bool {
	return len(job.FileDeletes) > 0
}

func (job Job) hasConfirmedFileDelete() bool {
	return slices.ContainsFunc(job.FileDeletes, func(record FileDelete) bool { return !record.unconfirmed() })
}

// hasConfirmedSearchOrGrab reports a search or a release grab the Arr is known
// to have taken. A blocklist alone is not one: whether the Arr then searches
// depends on its settings.
func (job Job) hasConfirmedSearchOrGrab() bool {
	return slices.ContainsFunc(job.Mutations, func(mutation Mutation) bool {
		return mutation.State == MutationConfirmed && mutation.Kind != MutationHistoryFailed
	})
}

// CompensateSearch puts back what a job that deleted an Arr file still owes:
// the episodes monitored again and one search of its own. It deletes nothing.
// The search is journaled under its own key, so a job that searched already
// still sends it, and a second call sends nothing more.
func (handler *arrHandler) CompensateSearch(ctx context.Context, job Job, progress JobProgress) error {
	instance, bindings, err := handler.target(job)
	if err != nil {
		return err
	}
	for _, record := range job.FileDeletes {
		if record.remonitorOwed() {
			if settleErr := handler.settleFileDelete(ctx, instance, &job, progress, record); settleErr != nil {
				return settleErr
			}
		}
	}
	mutation, err := searchMutation(instance, bindings)
	if err != nil {
		return err
	}
	mutation.Key += compensatingKeySuffix
	mutation, err = ensureMutationIntent(&job, progress, StatusSearching, mutation)
	if err != nil {
		return err
	}
	return handler.sendSearch(ctx, instance, &job, progress, mutation)
}

// stopWithCompensation ends a run that cannot go on after the job deleted an
// Arr file. It sends the compensating search, then leaves the job for an
// operator with the cause, the search's outcome and the re-monitor result.
func (s *Service) stopWithCompensation(
	ctx context.Context,
	handler Handler,
	id string,
	progress JobProgress,
	cause error,
	from func(Status) bool,
) bool {
	searchNote := "no compensating search was sent"
	if job, ok := s.Job(id); ok {
		if compensator, can := handler.(searchCompensator); can {
			compensateCtx, cancel := context.WithTimeout(ctx, executionTimeout)
			compensateErr := compensator.CompensateSearch(compensateCtx, job, progress)
			cancel()
			searchNote = "a compensating search was sent"
			if compensateErr != nil {
				searchNote = fmt.Sprintf("the compensating search did not complete: %v", compensateErr)
			}
		}
	}
	if job, ok := s.Job(id); ok && from != nil && !from(job.Status) {
		// A replacement can be indexed while the search is sent, and indexing completes a waiting job: it is ready
		// then, which is the better ending. A job that moved elsewhere is left to whatever moved it.
		s.logger.Debug().Str("job_id", id).Str("status", string(job.Status)).
			Msg("Reacquire job completed while it was searched for; left as it is")
		return true
	}
	monitorNote := "episodes are monitored again"
	if job, ok := s.Job(id); ok {
		var owed []int
		for _, record := range job.FileDeletes {
			if record.remonitorOwed() {
				owed = append(owed, record.EpisodeIDs...)
			}
		}
		if len(owed) > 0 {
			monitorNote = fmt.Sprintf("episodes %v could not be monitored again", owed)
		}
	}
	return s.stopReconciliationFrom(id, fmt.Errorf("%w; %s; %s", cause, searchNote, monitorNote), from)
}

// heldProgress reports to a job but keeps it in one status, whatever status a step asks for.
type heldProgress struct {
	inner  JobProgress
	status Status
}

func (progress heldProgress) Update(_ Status, mutate func(*Job)) error {
	return progress.inner.Update(progress.status, mutate)
}

func (progress heldProgress) UpdateDurable(_ Status, mutate func(*Job)) error {
	return progress.inner.UpdateDurable(progress.status, mutate)
}

// maintenanceContext is the context of the service's own work: the one Start
// was given, or a background one before it.
func (s *Service) maintenanceContext() context.Context {
	s.lifecycleMu.RLock()
	defer s.lifecycleMu.RUnlock()
	if s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

// expireWaitingJob ends a job that waited for its replacement too long. A job
// that deleted an Arr file is searched for once more and left for an operator:
// failing it would leave the media missing with nothing looking for it.
//
// The caller read the job as waiting, but it may have moved since: a job that is no longer waiting is left alone,
// and every step that changes it checks that it still is.
func (s *Service) expireWaitingJob(ctx context.Context, id string) {
	cause := errors.New("replacement was not imported before the reacquisition timeout")
	job, ok := s.Job(id)
	if !ok || !job.Status.waiting() {
		return
	}
	if job.hasFileDelete() {
		// The job keeps its waiting status while it is searched for, and each update checks that it still has it.
		// A dispatchable status would let the worker run the job against the same Arr, or stop it at the
		// reconciliation deadline before the search is sent. In production the worker and this pass share one loop
		// and cannot overlap; holding the status makes that independent of the loop. Every held update runs
		// completeJobFromIndex, and so does a binding indexed from another goroutine, so a replacement that
		// arrives meanwhile completes the job; stopWithCompensation then leaves it ready.
		held := &serviceJobProgress{service: s, jobID: id, from: Status.waiting}
		s.stopWithCompensation(
			ctx,
			s.currentHandler(),
			id,
			heldProgress{inner: held, status: job.Status},
			cause,
			Status.waiting,
		)
		return
	}
	_, _ = s.updateJobWithDurability(id, StatusFailed, func(job *Job) {
		job.LastError = cause.Error()
	}, false, Status.waiting)
}
