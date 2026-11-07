package reacquire

import (
	"context"
	"fmt"
	"strings"
)

const remonitorGaveUpNote = "could not be monitored again"

// retryRemonitors pays the re-monitors that deleted files left owed. A job
// whose search succeeded goes on to wait, and nothing runs its handler again,
// so the debt is kept in its record and collected here. A job that may still
// be running is left to its handler.
func (s *Service) retryRemonitors(ctx context.Context) {
	for _, job := range s.Jobs() {
		if !job.Status.dispatchable() && job.hasOwedRemonitor() {
			s.retryRemonitor(ctx, job.ID)
		}
	}
}

func (job Job) hasOwedRemonitor() bool {
	for _, record := range job.FileDeletes {
		if record.remonitorOwed() {
			return true
		}
	}
	return false
}

// retryRemonitor tries the owed re-monitors of one job that are due. A job
// that is out of tries says which episodes stay unmonitored, in its last error
// and in the log, and is not asked about again.
func (s *Service) retryRemonitor(ctx context.Context, id string) {
	job, ok := s.Job(id)
	if !ok || s.arrs == nil {
		return
	}
	for _, record := range job.FileDeletes {
		switch {
		case !record.remonitorOwed():
		case record.RemonitorAttempts >= remonitorMaxAttempts:
			s.surfaceRemonitor(id, record)
		case record.remonitorDue(s.now()):
			s.remonitor(ctx, id, job.ArrName, record)
		}
	}
}

func (s *Service) remonitor(ctx context.Context, id, arrName string, record FileDelete) {
	callCtx, cancel := context.WithTimeout(ctx, executionTimeout)
	err := s.arrs.MonitorEpisodes(callCtx, arrName, record.EpisodeIDs)
	cancel()
	if err == nil {
		record.Remonitored = true
	} else {
		record.RemonitorAttempts++
		record.RemonitorAt = s.now()
	}
	if saveErr := s.saveFileDelete(id, record); saveErr != nil {
		s.logger.Warn().Err(saveErr).Str("job_id", id).Msg("Could not record a re-monitor of deleted episodes")
		return
	}
	if err != nil && record.RemonitorAttempts >= remonitorMaxAttempts {
		s.surfaceRemonitor(id, record)
	}
}

// surfaceRemonitor puts the episodes that stay unmonitored in the job's last
// error, once, and logs them as an error.
func (s *Service) surfaceRemonitor(id string, record FileDelete) {
	note := fmt.Sprintf("episodes %v %s after %d tries; monitor them in the Arr",
		record.EpisodeIDs, remonitorGaveUpNote, record.RemonitorAttempts)
	job, ok := s.Job(id)
	if !ok || strings.Contains(job.LastError, remonitorGaveUpNote) {
		return
	}
	s.logger.Error().Str("job_id", id).Ints("episode_ids", record.EpisodeIDs).Int("file_id", record.ArrFileID).
		Msg("Episodes of a deleted Arr file could not be monitored again; monitor them in the Arr")
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	current, found := s.jobs[id]
	if !found {
		return
	}
	if current.LastError != "" {
		note = current.LastError + "; " + note
	}
	current.LastError = note
	if err := s.jobRepository.Save(current); err != nil {
		s.logger.Warn().Err(err).Str("job_id", id).Msg("Could not record unmonitored episodes on the job")
		return
	}
	s.jobs[id] = current
}

// saveFileDelete replaces one file delete record of a job without touching
// anything else, status and update time included: the wait for a replacement
// is timed from the last real change.
func (s *Service) saveFileDelete(id string, record FileDelete) error {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	current, ok := s.jobs[id]
	if !ok {
		return fmt.Errorf("reacquire job %q not found", id)
	}
	current = cloneJob(current)
	setFileDelete(&current, record)
	if err := s.jobRepository.SaveDurable(current); err != nil {
		return err
	}
	s.jobs[id] = current
	return nil
}
