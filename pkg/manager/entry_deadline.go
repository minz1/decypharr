package manager

import (
	"cmp"
	"context"
	"strings"
	"time"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// deadlineView is what grab.NextDeadline reads for key's grab besides its state.
func (a *entryActor) deadlineView(key string, st *entryState) grab.DeadlineView {
	g, polled := st.g, a.grabs.polledOf(key)
	v := grab.DeadlineView{
		Pending: polled.pending, Dead: polled.dead, StalledAfter: a.removeStalledAfter, Retention: a.removeStalledAfter,
		SeenFailure: a.grabs.seenFailure(key), PackRunning: a.packRunning(g.State.PackKey),
	}
	if g.Entry == nil {
		return v
	}
	v.PendingBase = g.Entry.AddedOn
	if p := g.Entry.GetActiveProvider(); p != nil {
		v.PendingBase = cmp.Or(p.AddedAt, g.Entry.AddedOn)
		if p.DownloadedAt != nil {
			v.DownloadedAt = *p.DownloadedAt
		}
	}
	return v
}

// packRunning reports a pack still waiting for its seasons: a failed season of it is kept, so the pack counts it
// as ended instead of handing it on again.
func (a *entryActor) packRunning(pack string) bool {
	if pack == "" {
		return false
	}
	v, ok := a.grabs.get(pack)
	return ok && !terminalPhase(v.State.Phase)
}

// armDeadline arms the deadline timer for the next deadline of the grab, and only re-arms it when that moved. While a
// failed save waits for its retry it arms nothing: the retry redelivers the message, and a due deadline re-armed
// at once would retry the save with no backoff.
func (a *entryActor) armDeadline(key string, st *entryState, fx *keyed.Effects[string, entryMsg]) {
	if st.hasSave {
		return
	}
	_, due, ok := grab.NextDeadline(st.g.State, a.deadlineView(key, st))
	if !ok {
		a.cancelDeadline(st, fx)
		return
	}
	if st.hasDeadlineTimer && st.deadlineAt.Equal(due) {
		return
	}
	a.cancelDeadline(st, fx)
	st.deadlineTimer = fx.After(time.Until(due), entryMsg{deadline: true})
	st.hasDeadlineTimer, st.deadlineAt = true, due
}

func (a *entryActor) cancelDeadline(st *entryState, fx *keyed.Effects[string, entryMsg]) {
	if st.hasDeadlineTimer {
		fx.CancelTimer(st.deadlineTimer)
	}
	st.hasDeadlineTimer, st.deadlineAt = false, time.Time{}
}

// deadlineFired steps the grab's deadline event if it is still due: the clock is read again, so a wake that came
// for another reason, or a timer for a deadline that has since moved, does nothing. A deadline that fires while a
// progress observation waits in the mailbox wins: the observation then finds the grab failed and is refused.
func (a *entryActor) deadlineFired(key string, st *entryState, fx *keyed.Effects[string, entryMsg]) {
	st.hasDeadlineTimer, st.deadlineAt = false, time.Time{}
	ev, due, ok := grab.NextDeadline(st.g.State, a.deadlineView(key, st))
	if !ok || due.After(time.Now()) {
		return
	}
	ev.At = time.Now()
	a.step(key, st, eventMsg(ev), fx)
}

// arrSaw records that an Arr listing rendered the grab's failure, so its retention may start to run. A listing of
// another failure (the grab failed again since) says nothing about this one.
func (a *entryActor) arrSaw(key string, st *entryState, failedAt time.Time) {
	g := st.g
	seen := failedAt.Truncate(time.Second)
	if g.State.Phase != grab.PhaseFailed || !seen.Equal(g.State.Failure.At.Truncate(time.Second)) {
		return
	}
	a.grabs.markSeen(key, failedAt)
}

// expiredGrab releases what a grab that ran out its provider wait or its wait for links still holds at the provider:
// the slot, and for a wait on the provider the torrent itself.
func (m *Manager) expiredGrab(g grab.Grab, kind grab.EventKind) {
	entry := g.Entry
	if entry == nil {
		return
	}
	provider := entry.ActiveProvider
	m.slotFreed(provider)
	placement := entry.GetActiveProvider()
	if kind != grab.EvPendingDeadline || placement == nil || placement.ID == "" {
		return
	}
	id := placement.ID
	m.startPlacementRun(entry.Name, provider, func(ctx context.Context) {
		m.discardSubmitted(ctx, provider, id, entry.InfoHash)
	})
}

// removeStalledAfter is the configured limit on a stalled placement and on a failed grab's retention; an invalid or
// non-positive setting is no limit.
func (m *Manager) removeStalledAfter() time.Duration {
	setting := m.config.RemoveStalledAfter
	if setting == "" {
		return 0
	}
	limit, err := utils.ParseDuration(setting)
	if err != nil {
		m.logger.Warn().Err(err).Str("remove_stalled_after", setting).
			Msg("Invalid remove_stalled_after; stalled entries are kept")
		return 0
	}
	return max(limit, 0)
}

// ArrSeen records that an Arr listing rendered these entries. A failed grab's retention starts to run once a listing
// rendered its failure; the grab is sent the failure time rendered, and only if it has not been told that one.
func (m *Manager) ArrSeen(rendered []*storage.Entry) {
	if m.entries == nil {
		return
	}
	for _, e := range rendered {
		if e.State != storage.EntryStateError {
			continue
		}
		key := strings.ToLower(e.InfoHash)
		var last time.Time
		if e.LastErrorTime != nil {
			last = *e.LastErrorTime
		}
		failedAt := cmp.Or(last, e.AddedOn, e.UpdatedAt)
		if failedAt.IsZero() {
			continue
		}
		if m.grabs.seenFailure(key).Equal(failedAt.Truncate(time.Second)) {
			continue
		}
		if err := m.entries.send(key, entryMsg{arrSaw: failedAt}); err != nil {
			m.logger.Debug().Err(err).Str("key", key).Msg("Arr listing not delivered to the grab")
		}
	}
}
