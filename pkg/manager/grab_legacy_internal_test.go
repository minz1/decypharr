package manager

import (
	"strings"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// p1Drives is what the previous binary does with a queue record: restore resubmits it, the poll drives it, or
// the stalled sweep removes it.
type p1Drives struct{ resubmit, poll, sweep bool }

// previousBinaryDrives runs the previous binary's own predicates on e: the restore snapshot filter and its
// Status==queued resubmit, its poll predicate (downloading, not queued, not local; a provider torrent or an NZB)
// and Queue.stalled with a job pending for every record
// (restore gave the queued ones one).
func previousBinaryDrives(e *storage.Entry, now time.Time) p1Drives {
	swept := previousSweepRemoves(e, now, time.Second)
	return p1Drives{
		resubmit: e.State == storage.EntryStateDownloading && e.Status == storage.EntryStatusQueued,
		poll: e.State == storage.EntryStateDownloading && e.Status != storage.EntryStatusQueued &&
			!e.IsDownloading && (e.IsTorrent() && e.ActiveProvider != "" || e.IsNZB()),
		sweep: swept,
	}
}

// previousSweepRemoves is the previous binary's stalled predicate with a job pending for every record: a failed record
// with no progress older than its retention (24h at least for an NZB), or an idle downloading one with no seeders
// and no progress older than after.
func previousSweepRemoves(t *storage.Entry, now time.Time, after time.Duration) bool {
	if t.State == storage.EntryStateError {
		failedAt := t.AddedOn
		if t.LastErrorTime != nil {
			failedAt = *t.LastErrorTime
		}
		retention := after
		if t.IsNZB() {
			retention = max(retention, 24*time.Hour)
		}
		return t.Progress == 0 && failedAt.Before(now.Add(-retention))
	}
	if t.IsDownloading || !t.AddedOn.Before(now.Add(-after)) || t.Status == storage.EntryStatusQueued {
		return false
	}
	return t.Status != storage.EntryStatusDownloading && t.Seeders == 0 && t.Progress == 0
}

// TestThePreviousBinaryDrivesEveryEncodedGrabAsItsOwnRecord pins the rollback contract of the legacy tuple: each
// phase's record lands where the previous binary's own record of that kind lands (spec §3.1).
func TestThePreviousBinaryDrivesEveryEncodedGrabAsItsOwnRecord(t *testing.T) {
	t.Parallel()
	const pack = "0123456789abcdef0123456789abcdef01234567"
	now := time.Now()
	failedAt := now.Add(-2 * time.Hour) // past the torrent retention (1s), within the NZB one (24h)
	record := func(kind string) *storage.Entry {
		e := &storage.Entry{
			InfoHash: pack, Name: "Show.S01", Protocol: config.ProtocolTorrent,
			State: storage.EntryStateDownloading, Status: storage.EntryStatusDownloading,
			ActiveProvider: "rd", AddedOn: now.Add(-48 * time.Hour),
			Providers: map[string]*storage.ProviderEntry{"rd": {Provider: "rd", ID: "RD1"}},
		}
		switch kind {
		case "nzb":
			e.Protocol = config.ProtocolNZB
		case "season":
			e.InfoHash = grab.SeasonHash(pack, 1)
		}
		return e
	}
	state := func(e *storage.Entry, kind string, p grab.Phase) grab.State {
		st := grab.State{Key: strings.ToLower(e.InfoHash), Phase: p, PhaseSince: now, NZB: kind == "nzb"}
		if kind == "season" {
			st.PackKey = pack
		}
		switch p {
		case grab.PhaseFailed:
			st.Failure = grab.Failure{Reason: "provider failed", At: failedAt, Count: 1}
		case grab.PhaseSubmitting:
			st.SubmitAttempt = grab.SubmitAttempt{Provider: "rd", At: now}
		case grab.PhaseDeleting:
			st.Cleanup = grab.Cleanup{DeleteFiles: true}
		case grab.PhaseAbsent, grab.PhaseQueued, grab.PhaseThrottled, grab.PhaseProvider, grab.PhaseAwaitingLinks,
			grab.PhaseActing, grab.PhaseComplete:
		}
		return st
	}
	var (
		resubmit = p1Drives{resubmit: true}
		poll     = p1Drives{poll: true}
		sweep    = p1Drives{sweep: true}
		idle     = p1Drives{}
	)
	cases := []struct {
		kind  string
		phase grab.Phase
		want  p1Drives
	}{
		{"torrent", grab.PhaseQueued, resubmit},
		{"torrent", grab.PhaseThrottled, resubmit},
		{"torrent", grab.PhaseSubmitting, resubmit},
		{"torrent", grab.PhaseProvider, poll},
		{"torrent", grab.PhaseAwaitingLinks, poll},
		{"torrent", grab.PhaseActing, poll},
		{"torrent", grab.PhaseComplete, idle},
		{"torrent", grab.PhaseFailed, sweep},
		{"torrent", grab.PhaseDeleting, poll}, // keeps the tuple it had
		{"nzb", grab.PhaseQueued, resubmit},
		{"nzb", grab.PhaseProvider, poll},
		{"nzb", grab.PhaseActing, poll},
		{"nzb", grab.PhaseComplete, idle},
		{"nzb", grab.PhaseFailed, idle}, // the NZB retention is a day
		{"season", grab.PhaseActing, idle},
		{"season", grab.PhaseComplete, idle},
		{"season", grab.PhaseFailed, sweep},
	}
	for _, c := range cases {
		e := record(c.kind)
		e.IsDownloading = true // a flag the previous run left; only a live Acting download keeps it
		if err := grab.Encode(grab.Grab{State: state(e, c.kind, c.phase), Entry: e}); err != nil {
			t.Fatalf("%s %v: Encode: %v", c.kind, c.phase, err)
		}
		if c.phase != grab.PhaseActing && c.phase != grab.PhaseDeleting && e.IsDownloading {
			t.Errorf("%s %v: IsDownloading left set", c.kind, c.phase)
		}
		e.IsDownloading = false // as restore resets it after the crash
		if got := previousBinaryDrives(e, now); got != c.want {
			t.Errorf("%s %v: the previous binary %+v, want %+v (%s/%s progress %v)",
				c.kind, c.phase, got, c.want, e.State, e.Status, e.Progress)
		}
	}
}
