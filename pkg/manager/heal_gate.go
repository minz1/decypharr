package manager

import (
	"errors"
	"fmt"
	"slices"

	"github.com/sirrobot01/decypharr/pkg/arr/reacquire"
	"github.com/sirrobot01/decypharr/pkg/repair"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// healGate is the reacquire gate of one heal of units files: the repair breaker is asked only if the request would
// create a job that deletes an Arr file, and the admission is given back if none is created. Without a repair
// service there is no gate.
func (m *Manager) healGate(entryID, provider string, class repair.Class, units int) *reacquire.Gate {
	if m.repair == nil {
		return nil
	}
	return &reacquire.Gate{
		Admit:  func() error { return m.admitHeal(entryID, provider, class, units) },
		Refund: func() { m.refundHeal(entryID, provider, class, units) },
	}
}

func (m *Manager) refundHeal(entryID, provider string, class repair.Class, units int) {
	if m.repair == nil {
		return
	}
	if provider == "" {
		provider = m.repair.ProviderFor(entryID)
	}
	m.repair.Refund(repair.HealRequest{Provider: provider, Class: class, Subject: entryID, Units: units})
}

// entryGate admits the reacquisitions of one entry's files together: the first file that would create a job asks
// the breaker for the files estimated to need one, and a file beyond the estimate asks for itself. What the files
// did not use is given back when the entry is done. Files that already have a job, or fail before one is created,
// ask for nothing and cost nothing.
type entryGate struct {
	m        *Manager
	entryID  string
	provider string
	class    repair.Class
	// estimate is how many files had no job when the entry was looked at; the first request asks for that many.
	estimate int

	err       error
	reserved  int
	requested int
}

func (g *entryGate) gate() *reacquire.Gate {
	if g.m.repair == nil {
		return nil
	}
	return &reacquire.Gate{
		Admit: func() error {
			if g.err != nil {
				return g.err
			}
			need := g.requested + 1
			if need > g.reserved {
				ask := max(g.estimate, need) - g.reserved
				if err := g.m.admitHeal(g.entryID, g.provider, g.class, ask); err != nil {
					g.err = err
					return err
				}
				g.reserved += ask
			}
			g.requested++
			return nil
		},
		Refund: func() { g.requested-- },
	}
}

// settle gives back the part of the admission no file used.
func (g *entryGate) settle() {
	if g.reserved > g.requested {
		g.m.refundHeal(g.entryID, g.provider, g.class, g.reserved-g.requested)
	}
}

// filesWithoutJob counts the files that have no active job. It only sizes the admission; the job queue decides
// whether a request really needs a job.
func filesWithoutJob(jobs []reacquire.Job, entryID string, files []string) int {
	count := 0
	for _, fileID := range files {
		if !slices.ContainsFunc(jobs, func(job reacquire.Job) bool {
			return job.EntryID == entryID && job.FileID == fileID && !job.Status.Terminal()
		}) {
			count++
		}
	}
	return count
}

// reacquireDroppedFiles queues a reacquisition for each of a dropped entry's files the Arr imported. It reports
// whether any was imported. The repair breaker is asked once for the entry, and only if one of its files would get a
// new job, so a sync that retries files which already have jobs, or that fail, asks nothing of it.
func (m *Manager) reacquireDroppedFiles(provider string, entry *storage.Entry, recovery ArrRecovery) (bool, error) {
	if recovery == nil || entry.InfoHash == "" {
		return false, nil
	}
	var bound []string
	for _, file := range entry.Files {
		if file.ID == "" {
			continue
		}
		if _, ok := recovery.Lookup(entry.InfoHash, file.ID); ok {
			bound = append(bound, file.ID)
		}
	}
	if len(bound) == 0 {
		return false, nil
	}
	gate := &entryGate{
		m: m, entryID: entry.InfoHash, provider: provider, class: repair.ClassDropped,
		estimate: filesWithoutJob(recovery.Jobs(), entry.InfoHash, bound),
	}
	defer gate.settle()
	var reacquireErr error
	for _, fileID := range bound {
		// A held heal is returned as an error, so the dropped entry is kept and retried on the next sync.
		if _, err := recovery.Reacquire(reacquire.Request{
			EntryID: entry.InfoHash,
			FileID:  fileID,
			Cause:   reacquire.CauseRepair,
			// A provider dropping a torrent is no evidence the release is bad: search again, blocklist nothing.
			Strategy: reacquire.StrategyCommandSearch,
			Gate:     gate.gate(),
		}); err != nil {
			reacquireErr = errors.Join(reacquireErr, fmt.Errorf("reacquire file %s: %w", fileID, err))
		}
	}
	if reacquireErr == nil {
		m.logger.Info().Str("debrid", provider).Str("infohash", entry.InfoHash).Str("name", entry.Name).
			Msg("Debrid dropped an imported torrent; reacquiring its files")
	}
	return true, reacquireErr
}
