package reacquire

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"uuid"

	"github.com/sirrobot01/decypharr/pkg/arr"
)

const (
	maintenanceInterval   = time.Minute
	executionTimeout      = 2 * time.Minute
	reconciliationTimeout = 30 * time.Minute
	waitingTimeout        = 6 * time.Hour
	successRetention      = 24 * time.Hour
	failureRetention      = 7 * 24 * time.Hour
	retryBaseDelay        = time.Second
	retryMaxDelay         = 30 * time.Second
	// retryMaxDoublings caps how often the retry delay doubles per attempt.
	retryMaxDoublings = 5
)

func (s *Service) Reacquire(request Request) (*Job, error) {
	release, err := s.beginOperation()
	if err != nil {
		return nil, err
	}
	defer release()
	request.Strategy = request.Strategy.normalized()
	if validateErr := request.validate(); validateErr != nil {
		return nil, validateErr
	}
	binding, ok := s.index.Lookup(request.EntryID, request.FileID)
	if !ok {
		return nil, fmt.Errorf("%w: entry %q file %q", ErrBindingNotFound, request.EntryID, request.FileID)
	}
	if !binding.AuthorizesMutation() {
		return nil, fmt.Errorf("%w: entry %q file %q", ErrBindingUnsafe, request.EntryID, request.FileID)
	}
	return s.enqueue(request, binding)
}

// FailDownload queues a job that fails the Arr's grab of downloadID, for a
// download the Arr never imported. downloadID must be the form the Arr stored.
func (s *Service) FailDownload(arrName, downloadID, entryID string) (*Job, error) {
	if arrName == "" || downloadID == "" || entryID == "" {
		return nil, errors.New("arr name, download ID and entry ID are required")
	}
	release, err := s.beginOperation()
	if err != nil {
		return nil, err
	}
	defer release()

	key := jobKey{arrName: arrName, downloadID: downloadID}
	if job, requeued, requeueErr := s.requeueWaitingJob(key); requeued || requeueErr != nil {
		return job, requeueErr
	}
	return s.queueJob(key, nil, func(now time.Time) Job {
		return Job{
			ID:         uuid.New().String(),
			Status:     StatusQueued,
			Cause:      CauseRepair,
			Strategy:   StrategyDownloadFailed,
			ArrName:    arrName,
			EntryID:    entryID,
			DownloadID: downloadID,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
	}, nil)
}

// requeueWaitingJob runs a waiting HistoryFailed job for key again: the Arr
// grabbed its failed download again, and a rerun fails that grab too.
func (s *Service) requeueWaitingJob(key jobKey) (*Job, bool, error) {
	s.jobsMu.RLock()
	id := s.activeReacquisitions[key]
	strategy := s.jobs[id].Strategy
	s.jobsMu.RUnlock()
	if strategy != StrategyHistoryFailed {
		return nil, false, nil
	}
	if err := s.rebindJob(id, nil); errors.Is(err, errNotWaiting) {
		return nil, false, nil
	} else if err != nil {
		return nil, true, err
	}
	s.signal()
	requeued, _ := s.Job(id)
	return &requeued, true, nil
}

// enqueue queues the job for binding and the other files of the same download.
//
// Invariant: the Arr files a job deletes are exactly the imported files of the managed entries it invalidates.
// Only bindings that authorize a mutation are kept, so a file bound any weaker is not deleted, and the invalidator
// (see InvalidationChecker) refuses an entry that has such a file instead of deleting its data under the Arr file.
func (s *Service) enqueue(request Request, binding Binding) (*Job, error) {
	// An unindexed file can become indexed while its replacement is pending.
	// Keep both request paths attached to the existing job.
	attached := func() (Job, bool) {
		for _, id := range s.activeReacquisitions {
			active := s.jobs[id]
			for _, existing := range active.Bindings {
				if existing.ArrName == binding.ArrName && existing.ArrFileID == binding.ArrFileID &&
					existing.ArrInstanceFingerprint == binding.ArrInstanceFingerprint {
					return cloneJob(active), true
				}
			}
		}
		return Job{}, false
	}
	return s.queueJob(keyForBinding(binding), attached, func(now time.Time) Job {
		var bindings []Binding
		if binding.DownloadID != "" {
			bindings = s.index.ByDownloadID(binding.ArrName, binding.DownloadID)
		}
		bindings = slices.DeleteFunc(bindings, func(binding Binding) bool {
			return !binding.AuthorizesMutation()
		})
		if len(bindings) == 0 {
			bindings = []Binding{binding}
		}
		return Job{
			ID:         uuid.New().String(),
			Status:     StatusQueued,
			Cause:      request.Cause,
			Strategy:   request.Strategy,
			ArrName:    binding.ArrName,
			ArrType:    binding.ArrType,
			EntryID:    request.EntryID,
			FileID:     request.FileID,
			DownloadID: binding.DownloadID,
			Bindings:   bindings,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
	}, request.Gate)
}

// existingJobLocked returns the active job for key, or the one attached reports. Caller must hold jobsMu.
func (s *Service) existingJobLocked(key jobKey, attached func() (Job, bool)) (Job, bool) {
	if id, exists := s.activeReacquisitions[key]; exists {
		return cloneJob(s.jobs[id]), true
	}
	if attached != nil {
		return attached()
	}
	return Job{}, false
}

// queueJob returns the active job for key (or the one attached reports),
// otherwise saves and indexes the job build returns and wakes the worker.
// attached and build run under jobsMu. A gate is asked once it is clear a job would be created, outside jobsMu; if
// a job appears meanwhile that one is returned and the admission refunded.
func (s *Service) queueJob(
	key jobKey, attached func() (Job, bool), build func(now time.Time) Job, gate *Gate,
) (*Job, error) {
	s.jobsMu.Lock()
	if job, ok := s.existingJobLocked(key, attached); ok {
		s.jobsMu.Unlock()
		return &job, nil
	}
	if gate != nil && gate.Admit != nil {
		s.jobsMu.Unlock()
		if err := gate.Admit(); err != nil {
			return nil, err
		}
		s.jobsMu.Lock()
		if job, ok := s.existingJobLocked(key, attached); ok {
			s.jobsMu.Unlock()
			gate.refund()
			return &job, nil
		}
	}
	job := build(s.now())
	if err := s.jobRepository.Save(job); err != nil {
		s.jobsMu.Unlock()
		gate.refund()
		return nil, err
	}
	s.jobs[job.ID] = cloneJob(job)
	s.activeReacquisitions[key] = job.ID
	s.jobsMu.Unlock()

	s.signal()
	result := cloneJob(job)
	return &result, nil
}

func (s *Service) Jobs() []Job {
	s.jobsMu.RLock()
	defer s.jobsMu.RUnlock()

	jobs := make([]Job, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, cloneJob(job))
	}
	slices.SortFunc(jobs, func(left, right Job) int {
		if result := right.UpdatedAt.Compare(left.UpdatedAt); result != 0 {
			return result
		}
		return cmp.Compare(left.ID, right.ID)
	})
	return jobs
}

func (s *Service) Job(id string) (Job, bool) {
	s.jobsMu.RLock()
	defer s.jobsMu.RUnlock()
	job, ok := s.jobs[id]
	return cloneJob(job), ok
}

// DeleteJobs removes completed job history from memory and the persisted job repository.
// Active jobs are rejected as a batch: deleting one would hide work that can
// still mutate an Arr instance, and deleting only part of a requested batch is
// surprising to callers.
func (s *Service) DeleteJobs(ids []string) (int, error) {
	release, err := s.beginOperation()
	if err != nil {
		return 0, err
	}
	defer release()

	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()

	uniqueIDs := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		job, exists := s.jobs[id]
		if !exists {
			continue
		}
		if !job.Status.Terminal() {
			return 0, fmt.Errorf("%w: job %q is %s", ErrJobNotTerminal, id, job.Status)
		}
		uniqueIDs = append(uniqueIDs, id)
	}

	deleted := 0
	for _, id := range uniqueIDs {
		if deleteErr := s.jobRepository.Delete(id); deleteErr != nil {
			return deleted, fmt.Errorf("delete reacquire job %q: %w", id, deleteErr)
		}
		delete(s.jobs, id)
		deleted++
	}
	return deleted, nil
}

func (s *Service) loadJobs(jobs []Job) error {
	slices.SortFunc(jobs, func(left, right Job) int {
		return left.UpdatedAt.Compare(right.UpdatedAt)
	})
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	clear(s.jobs)
	clear(s.activeReacquisitions)
	for _, job := range jobs {
		if job.Strategy == "" {
			if err := s.convertLegacyStrategy(&job); err != nil {
				return err
			}
		}
		s.jobs[job.ID] = cloneJob(job)
		if job.Status.Terminal() {
			continue
		}
		key := keyForJob(job)
		if previousID, exists := s.activeReacquisitions[key]; exists {
			previous := s.jobs[previousID]
			previous.Status = StatusCancelled
			previous.LastError = "superseded by duplicate active job"
			previous.UpdatedAt = s.now()
			previous.CompletedAt = previous.UpdatedAt
			if err := s.jobRepository.Save(previous); err != nil {
				return err
			}
			s.jobs[previousID] = previous
		}
		s.activeReacquisitions[key] = job.ID
	}
	return nil
}

// convertLegacyStrategy gives a stored job that has no strategy the one it can
// safely finish with, and stores it. Older binaries read an empty strategy as
// history_failed. A job that already blocklisted a release keeps that strategy
// so the blocklist stays settled; any other job searches and blocklists nothing.
func (s *Service) convertLegacyStrategy(job *Job) error {
	job.Strategy = StrategyCommandSearch
	if slices.ContainsFunc(job.Mutations, func(mutation Mutation) bool {
		return mutation.Kind == MutationHistoryFailed
	}) {
		job.Strategy = StrategyHistoryFailed
	}
	s.logger.Warn().
		Str("job_id", job.ID).
		Str("status", string(job.Status)).
		Str("strategy", string(job.Strategy)).
		Msg("Reacquire job was stored without a strategy; converted it")
	if err := s.jobRepository.Save(*job); err != nil {
		return fmt.Errorf("store converted reacquire job %q: %w", job.ID, err)
	}
	return nil
}

func (s *Service) run(ctx context.Context) {
	ticker := time.NewTicker(maintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileImportedJobs(ctx)
			s.maintainJobs()
			s.signal()
		case <-s.wake:
			if !s.drainJobs(ctx) {
				return
			}
		}
	}
}

// drainJobs runs dispatchable jobs until none is ready or one cannot be
// settled. It reports false once ctx is cancelled.
func (s *Service) drainJobs(ctx context.Context) bool {
	for {
		handler := s.currentHandler()
		if handler == nil {
			return true
		}
		job, ok := s.nextJob()
		if !ok {
			return true
		}
		if !s.runJob(ctx, handler, job) {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
	}
}

func (s *Service) runJob(ctx context.Context, handler Handler, job Job) bool {
	deadline := job.reconciliationDeadline()
	if !deadline.IsZero() && !s.now().Before(deadline) {
		return s.stopReconciliation(job.ID, errors.New("arr reconciliation deadline expired"))
	}
	started, err := s.updateJob(job.ID, StatusResolving, func(job *Job) {
		job.RetryAt = time.Time{}
	})
	if err != nil {
		return false
	}
	progress := &serviceJobProgress{service: s, jobID: job.ID}
	timeout := executionTimeout
	if !deadline.IsZero() {
		timeout = min(timeout, deadline.Sub(s.now()))
	}
	jobCtx, cancel := context.WithTimeout(ctx, timeout)
	err = handler.Reacquire(jobCtx, started, progress)
	if err == nil {
		err = jobCtx.Err()
	}
	cancel()
	if ctx.Err() != nil {
		return true
	}
	return s.settleJob(ctx, handler, job.ID, err, progress)
}

// settleJob records how one handler run ended: retry an unconfirmed mutation,
// stop for an operator, fail, or mark the job ready. It reports false when the
// job could not be updated.
func (s *Service) settleJob(
	ctx context.Context,
	handler Handler,
	id string,
	err error,
	progress *serviceJobProgress,
) bool {
	current, ok := s.Job(id)
	if !ok {
		return false
	}
	deadline := current.reconciliationDeadline()
	if err != nil && !deadline.IsZero() && !s.now().Before(deadline) {
		return s.stopReconciliation(id, fmt.Errorf("arr reconciliation deadline expired: %w", err))
	}
	if errors.Is(err, arr.ErrMutationOutcomeUnknown) || errors.Is(err, errArrLookup) {
		delay := retryDelay(current, err)
		if errors.Is(err, errArrLookup) {
			// No mutation attempts grow the delay, and a permanent error such
			// as a bad token is not backed off by the client: pace it.
			delay = retryMaxDelay
		}
		if !deadline.IsZero() {
			delay = min(delay, deadline.Sub(s.now()))
		}
		queued, updateErr := s.updateJobDurable(id, StatusQueued, func(job *Job) {
			job.LastError = err.Error()
			job.RetryAt = s.now().Add(delay)
		})
		if updateErr != nil {
			return false
		}
		s.scheduleRetry(queued)
		return true
	}
	if err != nil {
		return s.settleFailure(ctx, handler, current, err, progress)
	}
	current, ok = s.Job(id)
	if !ok || current.Status.Terminal() || progress.waiting.Load() || current.Status.waiting() {
		return true
	}
	s.retryRemonitor(ctx, id)
	_, err = s.updateJob(id, StatusReady, func(job *Job) {
		job.LastError = ""
		job.RetryAt = time.Time{}
	})
	return err == nil
}

// settleFailure ends a run that failed for a reason a retry does not mend. A job with a remote action that may have
// landed, or one that asks for an operator, stops for one; so does a job that deleted Arr files and has no search
// taken, but only after it sends the search. Any other job fails.
func (s *Service) settleFailure(
	ctx context.Context,
	handler Handler,
	current Job,
	err error,
	progress *serviceJobProgress,
) bool {
	operator := errors.Is(err, ErrOperatorRequired)
	if !operator && current.hasAttemptedIntent() {
		return s.stopReconciliation(current.ID, err)
	}
	if current.hasConfirmedFileDelete() && !current.hasConfirmedSearchOrGrab() {
		// The file is gone and nothing is searching for it: search, then let an operator look.
		return s.stopWithCompensation(ctx, handler, current.ID, progress, err, statusGuard{})
	}
	if operator {
		return s.stopReconciliation(current.ID, err)
	}
	_, updateErr := s.updateJob(current.ID, StatusFailed, func(job *Job) {
		job.LastError = err.Error()
		job.RetryAt = time.Time{}
	})
	return updateErr == nil
}

func (s *Service) nextJob() (Job, bool) {
	s.jobsMu.RLock()
	defer s.jobsMu.RUnlock()
	var next Job
	found := false
	for _, id := range s.activeReacquisitions {
		job, ok := s.jobs[id]
		if !ok || !job.Status.dispatchable() || job.RetryAt.After(s.now()) {
			continue
		}
		if !found || job.UpdatedAt.Before(next.UpdatedAt) {
			next = cloneJob(job)
			found = true
		}
	}
	return next, found
}

func (s *Service) updateJob(id string, status Status, mutate func(*Job)) (Job, error) {
	return s.updateJobWithDurability(id, status, mutate, false, statusGuard{})
}

func (s *Service) updateJobDurable(id string, status Status, mutate func(*Job)) (Job, error) {
	return s.updateJobWithDurability(id, status, mutate, true, statusGuard{})
}

var errNotWaiting = errors.New("reacquire job is not waiting")

// statusGuard limits the statuses an update may move a job out of. The zero value allows any.
type statusGuard struct {
	// want names the statuses the guard allows, for the error that reports a refusal.
	want   string
	allows func(Status) bool
}

// fromWaiting allows only a job that is waiting for its replacement.
func fromWaiting() statusGuard {
	return statusGuard{want: "waiting", allows: Status.waiting}
}

func (guard statusGuard) permits(status Status) bool {
	return guard.allows == nil || guard.allows(status)
}

// unexpectedStatusError reports a guarded update that found the job in another status than the guard allows.
type unexpectedStatusError struct {
	jobID string
	want  string
	got   Status
}

func (err *unexpectedStatusError) Error() string {
	return fmt.Sprintf("reacquire job %q is %s, expected a %s status", err.jobID, err.got, err.want)
}

// Is keeps the callers that ask whether the job was waiting working: this error matches errNotWaiting.
func (err *unexpectedStatusError) Is(target error) bool {
	return target == errNotWaiting
}

// rebindJob queues a waiting job again with more bindings to replace.
func (s *Service) rebindJob(id string, bindings []Binding) error {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	original, ok := s.jobs[id]
	if !ok || !original.Status.waiting() {
		return fmt.Errorf("reacquire job %q: %w", id, errNotWaiting)
	}
	job := cloneJob(original)
	job.Bindings = append(job.Bindings, bindings...)
	job.Status = StatusQueued
	job.LastError = "arr grabbed the failed download again; failing that grab too"
	job.UpdatedAt = s.now()
	if err := s.jobRepository.SaveDurable(job); err != nil {
		return err
	}
	s.jobs[id] = job
	return nil
}

// updateJobWithDurability changes a job's status and fields in one step under the jobs lock. When from is set, the
// job must be in a status it accepts at that moment: a caller that read the job earlier cannot move one that has
// since gone elsewhere.
func (s *Service) updateJobWithDurability(
	id string,
	status Status,
	mutate func(*Job),
	durable bool,
	from statusGuard,
) (Job, error) {
	if !status.valid() {
		return Job{}, fmt.Errorf("invalid reacquire status %q", status)
	}
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()

	original, ok := s.jobs[id]
	if !ok {
		return Job{}, fmt.Errorf("reacquire job %q not found", id)
	}
	if !from.permits(original.Status) {
		return Job{}, &unexpectedStatusError{jobID: id, want: from.want, got: original.Status}
	}
	if original.Status.Terminal() && status != original.Status {
		return Job{}, fmt.Errorf("reacquire job %q is already %s", id, original.Status)
	}
	job := cloneJob(original)
	if mutate != nil {
		mutate(&job)
	}
	job.ID = original.ID
	job.Cause = original.Cause
	job.Strategy = original.Strategy
	job.ArrName = original.ArrName
	job.ArrType = original.ArrType
	job.EntryID = original.EntryID
	job.FileID = original.FileID
	job.DownloadID = original.DownloadID
	job.Bindings = cloneJob(original).Bindings
	job.CreatedAt = original.CreatedAt
	job.Status = status
	job.UpdatedAt = s.now()
	if status != StatusQueued && job.StartedAt.IsZero() {
		job.StartedAt = job.UpdatedAt
	}
	if status.Terminal() {
		job.CompletedAt = job.UpdatedAt
		job.RetryAt = time.Time{}
	}
	var err error
	if durable {
		err = s.jobRepository.SaveDurable(job)
	} else {
		err = s.jobRepository.Save(job)
	}
	if err != nil {
		return Job{}, err
	}
	s.jobs[id] = cloneJob(job)
	if status.Terminal() {
		key := keyForJob(job)
		if s.activeReacquisitions[key] == id {
			delete(s.activeReacquisitions, key)
		}
	}
	return cloneJob(job), nil
}

// completeWaitingJobs re-checks the waiting jobs an indexing change can satisfy.
// It takes the whole change set at once: a generation replace carries every
// binding of an arr.Arr, and checking one binding at a time walks the job table
// once per binding.
func (s *Service) completeWaitingJobs(bindings ...Binding) error {
	downloadsByArr := make(map[string]map[string]struct{})
	for _, binding := range bindings {
		if binding.DownloadID == "" {
			continue
		}
		downloads, ok := downloadsByArr[binding.ArrName]
		if !ok {
			downloads = make(map[string]struct{})
			downloadsByArr[binding.ArrName] = downloads
		}
		downloads[binding.DownloadID] = struct{}{}
	}
	if len(downloadsByArr) == 0 {
		return nil
	}

	s.jobsMu.RLock()
	ids := make([]string, 0)
	for _, id := range s.activeReacquisitions {
		job, ok := s.jobs[id]
		if !ok || !job.Status.waiting() {
			continue
		}
		if replacesDownload(downloadsByArr[job.ArrName], job.DownloadID) {
			ids = append(ids, id)
		}
	}
	s.jobsMu.RUnlock()
	for _, id := range ids {
		job, ok := s.Job(id)
		if !ok {
			continue
		}
		if err := s.completeJobFromIndex(job); err != nil {
			return err
		}
	}
	return nil
}

type serviceJobProgress struct {
	service *Service
	jobID   string
	waiting atomic.Bool
	// from, when set, limits the statuses an update may move the job out of.
	from statusGuard
}

func (progress *serviceJobProgress) Update(status Status, mutate func(*Job)) error {
	return progress.update(status, mutate, false)
}

func (progress *serviceJobProgress) UpdateDurable(status Status, mutate func(*Job)) error {
	return progress.update(status, mutate, true)
}

func (progress *serviceJobProgress) update(status Status, mutate func(*Job), durable bool) error {
	if status.waiting() {
		progress.waiting.Store(true)
	}
	job, err := progress.service.updateJobWithDurability(progress.jobID, status, mutate, durable, progress.from)
	if err != nil || !status.waiting() {
		return err
	}
	return progress.service.completeJobFromIndex(job)
}

func retryDelay(job Job, err error) time.Duration {
	attempts := 0
	for _, mutation := range job.Mutations {
		attempts = max(attempts, mutation.Attempts)
	}
	for _, record := range job.FileDeletes {
		if record.unconfirmed() {
			attempts = max(attempts, record.Attempts)
		}
	}
	delay := retryBaseDelay
	for range max(0, min(attempts-1, retryMaxDoublings)) {
		delay *= 2
	}
	return min(retryMaxDelay, max(delay, arr.MutationRetryAfter(err)))
}

func (s *Service) scheduleRetry(job Job) {
	if job.RetryAt.IsZero() || s.ctx == nil {
		return
	}
	delay := max(time.Duration(0), job.RetryAt.Sub(s.now()))
	s.wg.Go(func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-s.ctx.Done():
		case <-timer.C:
			s.signal()
		}
	})
}

func (s *Service) schedulePersistedRetries() {
	for _, job := range s.Jobs() {
		if job.Status.dispatchable() && job.RetryAt.After(s.now()) {
			s.scheduleRetry(job)
		}
	}
}

// replacesDownload reports whether any indexed download differs from the one
// the job is replacing, which is what makes the job worth re-checking.
func replacesDownload(downloads map[string]struct{}, jobDownloadID string) bool {
	if len(downloads) == 0 {
		return false
	}
	if len(downloads) > 1 {
		return true
	}
	_, sameOnly := downloads[jobDownloadID]
	return !sameOnly
}

func (status Status) waiting() bool {
	return status == StatusWaitingForGrab || status == StatusWaitingForDownload || status == StatusWaitingForImport
}

func (status Status) dispatchable() bool {
	return status == StatusQueued || status == StatusResolving || status == StatusInvalidating ||
		status == StatusBlocklisting ||
		status == StatusSearching
}

func keyForBinding(binding Binding) jobKey {
	if binding.DownloadID != "" {
		return jobKey{arrName: binding.ArrName, downloadID: binding.DownloadID}
	}
	return jobKey{arrName: binding.ArrName, entryID: binding.EntryID, fileID: binding.EntryFileID}
}

func keyForJob(job Job) jobKey {
	if job.DownloadID != "" {
		return jobKey{arrName: job.ArrName, downloadID: job.DownloadID}
	}
	return jobKey{arrName: job.ArrName, entryID: job.EntryID, fileID: job.FileID}
}

func (s *Service) completeJobFromIndex(job Job) error {
	replacementDownloadIDs, ready := s.replacementsForJob(job)
	if !ready {
		return nil
	}
	s.retryRemonitor(s.maintenanceContext(), job.ID)
	_, err := s.updateJob(job.ID, StatusReady, func(job *Job) {
		job.ReplacementDownloadIDs = replacementDownloadIDs
		job.ReplacementDownloadID = replacementDownloadIDs[0]
		job.LastError = ""
	})
	return err
}

func (s *Service) replacementsForJob(job Job) ([]string, bool) {
	if len(job.Bindings) == 1 && job.Bindings[0].Confidence == ConfidenceLibraryFile {
		return nil, false
	}
	downloads := make(map[string]struct{})
	for _, target := range job.Bindings {
		var candidates []Binding
		switch {
		case target.MovieID > 0:
			candidates = s.index.ByMovieID(job.ArrName, target.MovieID)
		case len(target.EpisodeIDs) > 0:
			for _, episodeID := range target.EpisodeIDs {
				episodeCandidates := s.index.ByEpisodeID(job.ArrName, episodeID)
				candidate, found := replacementCandidate(
					episodeCandidates,
					job.DownloadID,
					func(candidate Binding) bool {
						return candidate.SeriesID == target.SeriesID
					},
				)
				if !found {
					return nil, false
				}
				downloads[candidate.DownloadID] = struct{}{}
			}
			continue
		case target.SeriesID > 0:
			candidates = s.index.BySeriesID(job.ArrName, target.SeriesID)
		default:
			return nil, false
		}

		candidate, found := replacementCandidate(candidates, job.DownloadID, func(candidate Binding) bool {
			if target.MovieID > 0 {
				return candidate.MovieID == target.MovieID
			}
			return candidate.SeriesID == target.SeriesID && candidate.SeasonNumber == target.SeasonNumber
		})
		if !found {
			return nil, false
		}
		downloads[candidate.DownloadID] = struct{}{}
	}
	if len(downloads) == 0 {
		return nil, false
	}
	result := make([]string, 0, len(downloads))
	for downloadID := range downloads {
		result = append(result, downloadID)
	}
	slices.Sort(result)
	return result, true
}

func replacementCandidate(candidates []Binding, oldDownloadID string, matches func(Binding) bool) (Binding, bool) {
	for _, candidate := range candidates {
		if candidate.AuthorizesMutation() && candidate.DownloadID != "" && candidate.DownloadID != oldDownloadID &&
			matches(candidate) {
			return candidate, true
		}
	}
	return Binding{}, false
}

func (s *Service) maintainJobs() {
	now := s.now()
	var expire []string
	var prune []string

	s.jobsMu.RLock()
	for id, job := range s.jobs {
		switch {
		case job.Status.waiting() && now.Sub(job.UpdatedAt) >= waitingTimeout:
			expire = append(expire, id)
		case job.Status == StatusFailed && terminalAge(now, job) >= failureRetention:
			prune = append(prune, id)
		case (job.Status == StatusReady || job.Status == StatusCancelled) && terminalAge(now, job) >= successRetention:
			prune = append(prune, id)
		}
	}
	s.jobsMu.RUnlock()

	for _, id := range expire {
		s.expireWaitingJob(s.maintenanceContext(), id)
	}
	s.retryRemonitors(s.maintenanceContext())
	for _, id := range prune {
		s.pruneTerminalJob(id, now)
	}
}

func (s *Service) pruneTerminalJob(id string, now time.Time) {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	job, ok := s.jobs[id]
	if !ok || !job.Status.Terminal() {
		return
	}
	retention := successRetention
	if job.Status == StatusFailed {
		retention = failureRetention
	}
	if terminalAge(now, job) < retention {
		return
	}
	if err := s.jobRepository.Delete(id); err == nil {
		delete(s.jobs, id)
	}
}

func terminalAge(now time.Time, job Job) time.Duration {
	at := job.CompletedAt
	if at.IsZero() {
		at = job.UpdatedAt
	}
	return now.Sub(at)
}
