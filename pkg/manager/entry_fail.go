package manager

import (
	"context"
	"errors"

	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
)

// failLater runs failEffects as a work task, so a slow Arr call never holds the grab's actor. A manager that is
// stopping starts nothing: the effects are dropped and logged.
func (m *Manager) failLater(g grab.Grab, effect grab.FailEffect) {
	if g.Entry != nil {
		g.Entry = g.Entry.Clone()
	}
	if err := m.runWork("grab-fail", func() { m.failEffects(g, effect) }); err != nil {
		m.logger.Warn().Err(err).Str("key", g.State.Key).Msg("Failed grab's Arr and NZB effects not run")
	}
}

// failEffects does what failing a grab does outside its record, which the actor has already marked failed: it
// drops a staged NZB, releases a failed NZB and tells the Arr; a bad release also fails the Arr's grab.
func (m *Manager) failEffects(g grab.Grab, effect grab.FailEffect) {
	entry := g.Entry
	if effect == grab.FailNoEffect || entry == nil {
		return
	}
	if err := removeStagedNZB(entry); err != nil {
		m.logger.Warn().Err(err).Str("name", entry.Name).Msg("Failed to remove the failed job's staged NZB")
	}
	if entry.IsNZB() && m.usenet != nil {
		if err := m.usenet.Fail(entry.InfoHash, errors.New(g.State.Failure.Reason)); err != nil {
			m.logger.Warn().Err(err).Str("name", entry.Name).Msg("Failed to release the failed NZB")
		}
	}
	m.refreshArr(entry)
	if effect != grab.FailBadRelease {
		return
	}
	grabErr := m.failReleaseGrab(entry)
	switch {
	case grabErr == nil:
	case errors.Is(grabErr, arr.ErrBlocklistHeld):
		key := g.State.Key
		m.holdGrab(entry.InfoHash, grabBucket(entry), func(context.Context) error {
			if v, ok := m.grabs.get(key); !ok || v.State.Phase != grab.PhaseFailed {
				return nil
			}
			return m.failReleaseGrab(entry)
		})
	default:
		m.logger.Warn().Err(grabErr).Str("name", entry.Name).Msg("Failed to fail the grab in the Arr")
	}
}
