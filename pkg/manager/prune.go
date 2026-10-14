package manager

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const (
	pruneExcludedManual     = "manual"
	pruneExcludedQueued     = "queued"
	pruneExcludedIncomplete = "incomplete"
	pruneExcludedReacquire  = "reacquire"
)

var ErrPruneDisabled = errors.New("prune is disabled")

type PruneCandidate struct {
	InfoHash, Name, Provider, Excluded string // Excluded: "" | "manual" | "queued" | "incomplete" | "reacquire"
	Size                               int64
	UnreferencedSince, EligibleAt      time.Time
}

type PruneReport struct {
	StartedAt                                                         time.Time
	Mode                                                              config.PruneMode
	Aborted                                                           string // "" when the run completed
	Links, Referenced, Dangling, NewlyUnreferenced, MissingReferenced int
	Candidates                                                        []PruneCandidate // all unreferenced entries, eligible or not
	Deleted, Failed                                                   int
	BytesFreed                                                        int64
}

const maxScanGap = 48 * time.Hour

func (c PruneCandidate) eligible(now time.Time) bool {
	return c.Excluded == "" && !now.Before(c.EligibleAt)
}

func (m *Manager) clockNow() time.Time {
	if m.pruneNow != nil {
		return m.pruneNow()
	}
	return time.Now()
}

func (m *Manager) LastPruneReport() PruneReport {
	m.pruneReportMu.Lock()
	defer m.pruneReportMu.Unlock()
	r := m.lastPrune
	r.Candidates = slices.Clone(r.Candidates)
	return r
}

func (m *Manager) RunPrune(ctx context.Context) (PruneReport, error) {
	m.pruneMu.Lock()
	defer m.pruneMu.Unlock()

	cfg := m.store.Get()
	if !cfg.Prune.Enabled {
		return PruneReport{}, ErrPruneDisabled
	}
	now := m.clockNow()
	report := PruneReport{StartedAt: now, Mode: cfg.Prune.Mode}
	err := m.pruneRun(ctx, cfg, now, &report)
	if err != nil && ctx.Err() != nil {
		return report, err
	}
	if err != nil {
		report.Aborted = err.Error()
		m.logger.Warn().Str("reason", report.Aborted).Msg("prune aborted")
	} else {
		m.logPruneSummary(report)
	}
	stored := report
	stored.Candidates = slices.Clone(report.Candidates)
	m.pruneReportMu.Lock()
	m.lastPrune = stored
	m.pruneReportMu.Unlock()
	return report, err
}

func (m *Manager) logPruneSummary(r PruneReport) {
	eligible := 0
	for _, c := range r.Candidates {
		if c.eligible(r.StartedAt) {
			eligible++
		}
	}
	m.logger.Info().
		Int("links", r.Links).
		Int("referenced", r.Referenced).
		Int("dangling", r.Dangling).
		Int("newly_unreferenced", r.NewlyUnreferenced).
		Int("candidates", len(r.Candidates)).
		Int("eligible", eligible).
		Int("deleted", r.Deleted).
		Int("failed", r.Failed).
		Int64("bytes_freed", r.BytesFreed).
		Str("mode", string(r.Mode)).
		Msg("prune finished")
}

func (m *Manager) pruneRun(ctx context.Context, cfg *config.Config, now time.Time, report *PruneReport) error {
	prune := cfg.Prune
	grace := prune.GraceDuration()
	if grace <= 0 {
		return fmt.Errorf("invalid prune grace %q", prune.Grace)
	}
	scan, err := scanReferences(prune.Roots, cfg.Mount.MountPath, m.lookupServedEntry)
	if err != nil {
		return fmt.Errorf("reference scan: %w", err)
	}
	report.Links, report.Referenced, report.Dangling = scan.Links, len(scan.Referenced), scan.Dangling
	if err = m.pruneSafetyChecks(scan); err != nil {
		return err
	}
	restart, gap, err := m.pruneScanGap(now, grace)
	if err != nil {
		return err
	}
	entries, ledger, err := m.pruneInventory()
	if err != nil {
		return err
	}
	report.MissingReferenced = missingReferenced(scan, entries)
	if report.MissingReferenced*100 > report.Referenced {
		return fmt.Errorf("%d of %d referenced entries are not in storage", report.MissingReferenced, report.Referenced)
	}
	if restart && len(ledger) > 0 {
		m.logger.Info().Dur("gap", gap).Msg("prune clocks restarted")
	}
	excluded, err := m.pruneExclusions(entries, scan)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = m.updateLedger(ctx, scan, entries, excluded, restart, ledger, now, report); err != nil {
		return err
	}
	report.Candidates = pruneCandidates(entries, ledger, excluded, scan, grace)
	if prune.Mode != config.PruneModeDelete {
		return nil
	}
	eligible := pickEligible(report.Candidates, now, prune.MaxPerRun)
	if len(eligible) == 0 {
		return nil
	}
	deleteFn := m.pruneEntries
	if m.pruneEntriesF != nil {
		deleteFn = m.pruneEntriesF
	}
	report.Deleted, report.Failed, report.BytesFreed = deleteFn(ctx, eligible)
	return nil
}

func (m *Manager) pruneSafetyChecks(scan referenceScan) error {
	referenced := len(scan.Referenced)
	if referenced == 0 {
		return errors.New("scan resolved 0 references")
	}
	last, ok, err := m.storage.LastReferencedCount()
	if err != nil {
		return fmt.Errorf("read last referenced count: %w", err)
	}
	if ok && referenced*10 < last*9 {
		return fmt.Errorf("referenced count %d is below 90%% of the last scan's %d", referenced, last)
	}
	return nil
}

func (m *Manager) pruneScanGap(now time.Time, grace time.Duration) (bool, time.Duration, error) {
	last, ok, err := m.storage.LastScan()
	if err != nil {
		return false, 0, fmt.Errorf("read last scan time: %w", err)
	}
	if !ok {
		return true, 0, nil
	}
	gap := now.Sub(last)
	return gap < 0 || gap > min(maxScanGap, grace), gap, nil
}

func missingReferenced(scan referenceScan, entries map[string]*storage.Entry) int {
	n := 0
	for hash := range scan.Referenced {
		if _, ok := entries[hash]; !ok {
			n++
		}
	}
	return n
}

func (m *Manager) pruneInventory() (map[string]*storage.Entry, map[string]time.Time, error) {
	ledger, err := m.storage.UnreferencedSince()
	if err != nil {
		return nil, nil, fmt.Errorf("read prune ledger: %w", err)
	}
	entries := map[string]*storage.Entry{}
	err = m.storage.ForEach(func(e *storage.Entry) error {
		if e.InfoHash != "" {
			entries[e.InfoHash] = e
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list entries: %w", err)
	}
	return entries, ledger, nil
}

func (m *Manager) pruneExclusions(
	entries map[string]*storage.Entry, scan referenceScan,
) (map[string]string, error) {
	reacquiring := map[string]struct{}{}
	recovery := m.recoveryService()
	if recovery == nil && m.arr != nil && len(m.arr.All()) > 0 {
		return nil, errors.New("arr recovery unavailable")
	}
	if recovery != nil {
		for _, job := range recovery.Jobs() {
			if !job.Status.Terminal() {
				reacquiring[job.EntryID] = struct{}{}
			}
		}
	}
	excluded := map[string]string{}
	for hash, e := range entries {
		if _, referenced := scan.Referenced[hash]; referenced {
			continue
		}
		if reason := m.pruneExclusion(e, reacquiring); reason != "" {
			excluded[hash] = reason
		}
	}
	return excluded, nil
}

func (m *Manager) updateLedger(
	ctx context.Context, scan referenceScan, entries map[string]*storage.Entry, excluded map[string]string,
	restart bool, ledger map[string]time.Time, now time.Time, report *PruneReport,
) error {
	var stale []string
	for hash := range ledger {
		_, referenced := scan.Referenced[hash]
		_, present := entries[hash]
		_, isExcluded := excluded[hash]
		if referenced || !present || isExcluded {
			stale = append(stale, hash)
		}
	}
	if err := m.storage.ClearUnreferencedSince(stale...); err != nil {
		return fmt.Errorf("clear prune ledger: %w", err)
	}
	for _, hash := range stale {
		delete(ledger, hash)
	}
	if restart {
		clear(ledger)
	}
	stamp := time.Unix(now.Unix(), 0)
	for hash := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, referenced := scan.Referenced[hash]
		_, isExcluded := excluded[hash]
		if _, stamped := ledger[hash]; referenced || isExcluded || stamped {
			continue
		}
		if err := m.storage.SetUnreferencedSince(hash, stamp); err != nil {
			return fmt.Errorf("stamp prune ledger: %w", err)
		}
		ledger[hash] = stamp
		report.NewlyUnreferenced++
	}
	if err := m.storage.SetLastReferencedCount(len(scan.Referenced)); err != nil {
		return fmt.Errorf("record referenced count: %w", err)
	}
	if err := m.storage.SetLastScan(now); err != nil {
		return fmt.Errorf("record scan time: %w", err)
	}
	return nil
}

func pruneCandidates(
	entries map[string]*storage.Entry, ledger map[string]time.Time, excluded map[string]string,
	scan referenceScan, grace time.Duration,
) []PruneCandidate {
	var out []PruneCandidate
	for hash, e := range entries {
		if _, referenced := scan.Referenced[hash]; referenced {
			continue
		}
		c := PruneCandidate{
			InfoHash: hash,
			Name:     e.Name,
			Provider: pruneProvider(e),
			Excluded: excluded[hash],
			Size:     e.Size,
		}
		if c.Excluded == "" {
			c.UnreferencedSince = ledger[hash]
			c.EligibleAt = c.UnreferencedSince.Add(grace)
		}
		out = append(out, c)
	}
	slices.SortFunc(out, compareCandidates)
	return out
}

func (m *Manager) pruneExclusion(e *storage.Entry, reacquiring map[string]struct{}) string {
	switch {
	case e.Manual:
		return pruneExcludedManual
	case m.pruneQueued(e.InfoHash):
		return pruneExcludedQueued
	case !e.IsComplete || e.State == storage.EntryStateDownloading:
		return pruneExcludedIncomplete
	}
	if _, ok := reacquiring[e.InfoHash]; ok {
		return pruneExcludedReacquire
	}
	return ""
}

func (m *Manager) pruneQueued(infohash string) bool {
	_, err := m.Queue().GetTorrent(infohash)
	return !errors.Is(err, appendstore.ErrKeyNotFound)
}

func pruneProvider(e *storage.Entry) string {
	if e.ActiveProvider != "" {
		return e.ActiveProvider
	}
	names := make([]string, 0, len(e.Providers))
	for name := range e.Providers {
		names = append(names, name)
	}
	slices.Sort(names)
	return strings.Join(names, ",")
}

func compareCandidates(a, b PruneCandidate) int {
	return cmp.Or(a.UnreferencedSince.Compare(b.UnreferencedSince), cmp.Compare(a.InfoHash, b.InfoHash))
}

func pickEligible(candidates []PruneCandidate, now time.Time, limit int) []PruneCandidate {
	var eligible []PruneCandidate
	for _, c := range candidates {
		if c.eligible(now) {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) > limit {
		eligible = eligible[:limit]
	}
	return eligible
}

func (m *Manager) pruneEntries(context.Context, []PruneCandidate) (int, int, int64) {
	return 0, 0, 0
}
