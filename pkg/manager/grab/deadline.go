package grab

import (
	"fmt"
	"time"
)

// Pending is what a provider's last status check says the placement is waiting on.
type Pending uint8

const (
	// PendingNone is a placement not waiting on its provider.
	PendingNone Pending = iota
	// PendingProvider is a provider that has not finished processing the torrent, or its links.
	PendingProvider
	// PendingConverting is a magnet the provider has not converted yet.
	PendingConverting
)

const (
	// PendingLimit is how long a placement may wait on a provider that has not finished processing its torrent
	// before the grab fails. The failure carries no verdict on the release.
	PendingLimit = 6 * time.Hour
	// ConversionLimit is how long a magnet may stay unconverted. One that never converts has no seeders, and
	// Real-Debrid drops it at about 3h on its own; failing it first, as a dead release, lets the Arr search
	// another one sooner.
	ConversionLimit = 2 * time.Hour
	// LinksLimit is how long a finished torrent may go without file links before its grab fails.
	LinksLimit = 6 * time.Hour
	// NZBRetention is the least time a failed NZB stays in SAB history: a backfilling arr can skip refreshes for
	// hours.
	NZBRetention = 24 * time.Hour
)

// DeadlineView is what NextDeadline reads besides the state: the timestamps of the record, what the last poll saw
// and what the actor knows of the Arr and the pack.
type DeadlineView struct {
	// PendingBase is when the placement was added, else the entry; zero when neither is known.
	PendingBase time.Time
	// Pending and Dead are what the last poll saw; Dead says the release itself is dead.
	Pending Pending
	Dead    bool
	// DownloadedAt is when the active placement finished downloading; zero when unknown.
	DownloadedAt time.Time
	// StalledAfter is how long a placement may stall; zero means no limit.
	StalledAfter time.Duration
	// Retention is how long a failed grab is kept after an Arr listed it; zero means forever.
	Retention time.Duration
	// SeenFailure is the failure time an Arr listing rendered, or zero.
	SeenFailure time.Time
	// PackRunning is a pack still waiting for this season.
	PackRunning bool
}

// NextDeadline is the next event the clock owes the grab, and when it is due; false when none is. It is pure: every
// deadline is derived from persisted timestamps and what the poll saw, so none is stored.
func NextDeadline(st State, v DeadlineView) (Event, time.Time, bool) {
	switch st.Phase {
	case PhaseProvider:
		return providerDeadline(st, v)
	case PhaseAwaitingLinks:
		if v.DownloadedAt.IsZero() {
			return Event{}, time.Time{}, false
		}
		return Event{
			Kind: EvLinkWaitExpired, Fail: FailBadRelease,
			Reason: fmt.Sprintf("provider gave no file links %s after the download finished", LinksLimit),
		}, v.DownloadedAt.Add(LinksLimit), true
	case PhaseFailed:
		return retentionDeadline(st, v)
	case PhaseAbsent, PhaseQueued, PhaseThrottled, PhaseSubmitting, PhaseActing, PhaseComplete, PhaseDeleting:
	}
	return Event{}, time.Time{}, false
}

func providerDeadline(st State, v DeadlineView) (Event, time.Time, bool) {
	var (
		ev  Event
		due time.Time
		ok  bool
	)
	if limit := pendingLimit(v.Pending); limit > 0 && !v.PendingBase.IsZero() {
		reason := fmt.Sprintf("provider still processing the placement %s after it was added", limit)
		ev, due, ok = Event{Kind: EvPendingDeadline, Fail: FailEntry, Reason: reason}, v.PendingBase.Add(limit), true
		if v.Dead {
			ev.Fail = FailBadRelease
		}
	}
	if !st.StalledSince.IsZero() && v.StalledAfter > 0 {
		if at := st.StalledSince.Add(v.StalledAfter); !ok || at.Before(due) {
			ev, due, ok = Event{Kind: EvStalledDeadline, Fail: FailNoEffect}, at, true
			if st.UnknownRaw != "" {
				ev.Reason = fmt.Sprintf("provider state %q unknown for %s", st.UnknownRaw, v.StalledAfter)
			}
		}
	}
	return ev, due, ok
}

func pendingLimit(p Pending) time.Duration {
	switch p {
	case PendingProvider:
		return PendingLimit
	case PendingConverting:
		return ConversionLimit
	case PendingNone:
	}
	return 0
}

func retentionDeadline(st State, v DeadlineView) (Event, time.Time, bool) {
	if v.Retention <= 0 || v.PackRunning || v.SeenFailure.IsZero() ||
		!v.SeenFailure.Truncate(time.Second).Equal(st.Failure.At.Truncate(time.Second)) {
		return Event{}, time.Time{}, false
	}
	retention := v.Retention
	if st.NZB {
		retention = max(retention, NZBRetention)
	}
	return Event{Kind: EvFailedRetention}, st.Failure.At.Add(retention), true
}
