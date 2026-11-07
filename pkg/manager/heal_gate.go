package manager

import (
	"errors"
	"fmt"
	"sync"

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
// the breaker for all of them, and what the files did not use is given back when the entry is done. Files that
// already have a job, or fail before one is created, ask for nothing and cost nothing.
type entryGate struct {
	m        *Manager
	entryID  string
	provider string
	class    repair.Class
	units    int

	once      sync.Once
	err       error
	asked     bool
	requested int
}

func (g *entryGate) gate() *reacquire.Gate {
	if g.m.repair == nil {
		return nil
	}
	return &reacquire.Gate{
		Admit: func() error {
			g.once.Do(func() {
				g.asked = true
				g.err = g.m.admitHeal(g.entryID, g.provider, g.class, g.units)
			})
			if g.err == nil {
				g.requested++
			}
			return g.err
		},
		Refund: func() { g.requested-- },
	}
}

// settle gives back the part of the admission no file used.
func (g *entryGate) settle() {
	if g.asked && g.err == nil && g.units > g.requested {
		g.m.refundHeal(g.entryID, g.provider, g.class, g.units-g.requested)
	}
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
	gate := &entryGate{m: m, entryID: entry.InfoHash, provider: provider, class: repair.ClassDropped, units: len(bound)}
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
