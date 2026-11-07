package reacquire

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// reconciliationDeadline uses the first intent time, or the first unconfirmed
// file delete, whichever is older. Retries do not extend it.
func (job Job) reconciliationDeadline() time.Time {
	var first time.Time
	for _, mutation := range job.Mutations {
		if mutation.State == MutationIntent && (first.IsZero() || mutation.IntentAt.Before(first)) {
			first = mutation.IntentAt
		}
	}
	for _, record := range job.FileDeletes {
		if record.unconfirmed() && (first.IsZero() || record.AttemptedAt.Before(first)) {
			first = record.AttemptedAt
		}
	}
	if first.IsZero() && len(job.Mutations) == 0 {
		first = job.StartedAt
	}
	if first.IsZero() {
		return time.Time{}
	}
	return first.Add(reconciliationTimeout)
}

// NeedsAttention is what an operator is told when a job stops for them.
type NeedsAttention struct {
	JobID   string
	ArrName string
	// File is the library file the job was replacing.
	File string
	// Why is the cause that stopped the job, with what the job did about it.
	Why string
	// Remonitor says where the re-monitor of the deleted files' episodes stands.
	Remonitor string
}

func (s *Service) stopReconciliation(id string, cause error) bool {
	return s.stopReconciliationFrom(id, cause, statusGuard{})
}

// stopReconciliationFrom is stopReconciliation for a job that must still be in a status from accepts: it stops
// nothing, and reports false, for one that has moved on.
func (s *Service) stopReconciliationFrom(id string, cause error, from statusGuard) bool {
	before, known := s.Job(id)
	stopped, err := s.updateJobWithDurability(id, StatusNeedsAttention, func(job *Job) {
		job.LastError = fmt.Sprintf(
			"%v; check the action in Arr, then acknowledge this job before starting another",
			cause,
		)
		job.RetryAt = time.Time{}
	}, true, from)
	if err != nil {
		return false
	}
	if s.onNeedsAttention != nil && known && before.Status != StatusNeedsAttention {
		s.onNeedsAttention(NeedsAttention{
			JobID:     stopped.ID,
			ArrName:   stopped.ArrName,
			File:      stopped.libraryFile(),
			Why:       cause.Error(),
			Remonitor: stopped.remonitorSummary(),
		})
	}
	return true
}

// libraryFile names the file the job is replacing.
func (job Job) libraryFile() string {
	for _, binding := range job.Bindings {
		if binding.EntryID == job.EntryID && binding.EntryFileID == job.FileID {
			return cmp.Or(binding.LibraryPath, binding.EntryFileName, job.FileID)
		}
	}
	return job.FileID
}

// remonitorSummary says where the episodes of the files the job deleted stand.
func (job Job) remonitorSummary() string {
	if !job.hasFileDelete() {
		return "no Arr file was deleted"
	}
	var unconfirmed, owed []string
	for _, record := range job.FileDeletes {
		switch {
		case record.unconfirmed():
			unconfirmed = append(unconfirmed, strconv.Itoa(record.ArrFileID))
		case record.remonitorOwed():
			owed = append(owed, fmt.Sprint(record.EpisodeIDs))
		}
	}
	switch {
	case len(unconfirmed) > 0:
		return fmt.Sprintf("the delete of Arr file %s is not confirmed, so its episodes may be unmonitored",
			strings.Join(unconfirmed, ", "))
	case len(owed) > 0:
		return fmt.Sprintf("episodes %s are not monitored again yet", strings.Join(owed, ", "))
	default:
		return "episodes are monitored again"
	}
}

// AcknowledgeJob releases a stopped job after an operator checks its remote actions.
// It retains the mutation record. It does not send another action to Arr.
func (s *Service) AcknowledgeJob(id string) (Job, error) {
	release, err := s.beginOperation()
	if err != nil {
		return Job{}, err
	}
	defer release()
	job, ok := s.Job(id)
	if !ok || job.Status != StatusNeedsAttention {
		return Job{}, ErrJobNotBlocked
	}
	return s.updateJobDurable(id, StatusCancelled, nil)
}
