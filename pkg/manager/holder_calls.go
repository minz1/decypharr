package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

var errNoHolder = errors.New("provider torrent holder is not running")

// placementRelease is one grab letting go of a provider torrent: delete asks for it to be deleted if nothing else
// holds it, slot says it held a download slot, and library says the grab's own library record is the one being
// removed, so it does not count as a holder.
type placementRelease struct {
	provider, id, grab    string
	delete, slot, library bool
}

// holderActor is the runner of provider torrent holders, on the placements group so it stops after the entry runner
// and before usenet and storage. A manager without that group has none and refuses every message.
func (m *Manager) holderActor() *holderActor {
	m.holdersOnce.Do(func() {
		if m.placementsSvc == nil {
			return
		}
		m.holders = newHolderActor(m.placementsSvc.group, holderDeps{
			logger:    m.logs.New("holder"),
			recount:   m.holderRecount,
			remove:    m.providerDelete,
			refusal:   m.placementRefusal,
			lock:      m.lockProvider,
			slotFreed: m.slotFreed,
			gone:      m.placementGone,
		})
	})
	return m.holders
}

func (m *Manager) lockProvider(provider string) func() {
	lock := m.providerLock(provider)
	lock.Lock()
	return lock.Unlock
}

func (m *Manager) sendHolder(provider, id string, ev holderEvent) error {
	h := m.holderActor()
	if h == nil || h.runner == nil {
		return errNoHolder
	}
	ev.grab = strings.ToLower(ev.grab)
	err := h.runner.Send(holderKey(provider, id), ev)
	if err != nil && m.placementsSvc.group.Context().Err() != nil {
		return fmt.Errorf("%w: %w", errPlacementStopped, err)
	}
	return err
}

func (m *Manager) holdPlacement(provider, id, grabKey string) error {
	return m.sendHolder(provider, id, holderEvent{kind: holderHold, grab: grabKey})
}

func (m *Manager) sendRelease(r placementRelease, done chan releaseResult) error {
	return m.sendHolder(r.provider, r.id, holderEvent{
		kind: holderRelease, grab: r.grab, del: r.delete, slot: r.slot, library: r.library, done: done,
	})
}

// releaseWait releases a provider torrent for a grab and waits for what became of it: it reports whether the torrent
// was deleted. It must run in a goroutine that is no actor's. Once the release is sent the wait ignores ctx: the delete runs on the stop grace, so a
// caller that gave up early would misreport a delete that still happens. Only the manager stopping ends it early.
func (m *Manager) releaseWait(ctx context.Context, r placementRelease) (bool, error) {
	if r.provider == usenetProvider {
		return m.releaseUsenet(ctx, r.id, r.grab)
	}
	if err := m.placementRefusal(ctx); err != nil {
		return false, err
	}
	done := make(chan releaseResult, 1)
	if err := m.sendRelease(r, done); err != nil {
		return false, err
	}
	var stop <-chan struct{}
	if m.placementsSvc != nil {
		stop = m.placementsSvc.group.Context().Done()
	}
	var graceEnded <-chan struct{}
	if m.grace != nil {
		graceEnded = m.grace.Done()
	}
	select {
	case res := <-done:
		return releaseError(res)
	case <-graceEnded:
		return false, fmt.Errorf("%w: %w", errPlacementStopped, context.Cause(m.grace))
	case <-stop:
		return false, fmt.Errorf("%w: %w", errPlacementStopped, m.placementsSvc.group.Context().Err())
	}
}

func releaseError(res releaseResult) (bool, error) {
	switch res.outcome {
	case releaseRemoved:
		return true, nil
	case releaseHeldElsewhere, releaseReleased:
		return false, nil
	case releaseStopped:
		if placementStopped(res.err) {
			return false, res.err
		}
		return false, fmt.Errorf("%w: %w", errPlacementStopped, res.err)
	case releaseFailed:
	}
	return false, res.err
}

// holderRecount is the recount before a delete: the holds the holder was given are checked first, then every record
// of the library and the queue. The releasing grab's queue record never counts, and its library record counts unless
// the release says it is the one being removed.
func (m *Manager) holderRecount(provider, id string, call holderCall, holds []string) (holderRecount, error) {
	var rc holderRecount
	if m.storage == nil {
		return rc, nil
	}
	lists := func(e *storage.Entry) bool {
		p := e.Providers[provider]
		return p != nil && p.ID == id
	}
	for _, h := range holds {
		e, err := m.storage.GetQueued(h)
		switch {
		case errors.Is(err, appendstore.ErrKeyNotFound):
			rc.stale = append(rc.stale, h)
		case err != nil:
			return holderRecount{}, err
		case lists(e):
			rc.held = true
		default:
			rc.stale = append(rc.stale, h)
		}
	}
	if rc.held {
		return rc, nil
	}
	own := func(e *storage.Entry) bool { return strings.EqualFold(e.InfoHash, call.releasing) }
	err := m.storage.ForEachStrict(func(e *storage.Entry) error {
		if lists(e) && (!call.library || !own(e)) {
			return errPlacementHeld
		}
		return nil
	})
	if errors.Is(err, errPlacementHeld) {
		rc.held = true
		return rc, nil
	}
	if err != nil {
		return holderRecount{}, err
	}
	queued, err := m.storage.FilterQueued(func(e *storage.Entry) bool { return lists(e) && !own(e) })
	rc.held = len(queued) > 0
	return rc, err
}

// placementGone tells a grab that tried to hold a torrent already deleted that its placement is gone.
func (m *Manager) placementGone(grabKey, provider, id string) {
	if m.entries == nil {
		return
	}
	msg := eventMsg(grab.Event{
		Kind: grab.EvPlacementGone, Fail: grab.FailNoEffect, Provider: provider, TorrentID: id,
		Reason: "the provider torrent was deleted",
	})
	if err := m.entries.send(strings.ToLower(grabKey), msg); err != nil {
		m.logger.Warn().Err(err).Str("key", grabKey).Str("provider", provider).Str("id", id).
			Msg("Grab not told its provider torrent was deleted")
	}
}
