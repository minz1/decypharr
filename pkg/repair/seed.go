package repair

import "github.com/sirrobot01/decypharr/pkg/arr/reacquire"

// jobLister is the part of the reacquirer that lists its journal.
type jobLister interface {
	Jobs() []reacquire.Job
}

// openReacquisitions returns the IDs of the entries a reacquisition still open in the journal covers, nil when the
// reacquirer keeps no journal. Their heal is under way: a sweep that finds them still broken has no new evidence.
// A job waiting on an operator, or open longer than the breaker Window, is no heal under way, so it hides nothing
// (SM12-44).
func (r *Service) openReacquisitions() map[string]bool {
	lister, ok := r.reacquirer.(jobLister)
	if !ok {
		return nil
	}
	open := make(map[string]bool)
	cutoff := r.clock().Add(-r.breakerSettings().Window)
	for _, job := range lister.Jobs() {
		if job.Status.Terminal() || job.Status == reacquire.StatusNeedsAttention || job.CreatedAt.Before(cutoff) {
			continue
		}
		open[job.EntryID] = true
		for _, binding := range job.Bindings {
			open[binding.EntryID] = true
		}
	}
	delete(open, "") // a library job may name no entry
	return open
}

// seedBreakerFromJournal restores the heals of the last window into the breaker after a restart, from the
// reacquire job journal, so a restart does not hand a tripped provider a fresh set of caps. The journal does not
// record why a repair job was queued, so a job counts toward its cause's bucket: usenet segments for a stream
// failure, the unclassified bucket for everything else. A job that fails a grab deletes no Arr file and is skipped.
func (r *Service) seedBreakerFromJournal() {
	// Start also runs on every config change; only the first call is a restart, later journal entries are
	// heals this process already counted.
	if r.seeded {
		return
	}
	r.seeded = true
	lister, ok := r.reacquirer.(jobLister)
	if !ok {
		return
	}
	now := r.clock()
	window := r.breakerSettings().Window
	for _, job := range lister.Jobs() {
		if job.Strategy == reacquire.StrategyDownloadFailed || now.Sub(job.CreatedAt) > window {
			continue
		}
		class := ClassOther
		if job.Cause == reacquire.CauseStream {
			class = ClassUsenetSegment
		}
		r.breaker.seed(job.CreatedAt, Bucket{Provider: r.ProviderFor(job.EntryID), Class: class}, job.EntryID)
	}
}
