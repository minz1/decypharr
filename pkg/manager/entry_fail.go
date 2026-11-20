package manager

import (
	"errors"

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

func (m *Manager) failEffects(g grab.Grab, effect grab.FailEffect) {
	entry := g.Entry
	if effect == grab.FailNoEffect || entry == nil {
		return
	}
	if err := m.queue.removeStagedNZB(entry); err != nil {
		m.logger.Warn().Err(err).Str("name", entry.Name).Msg("Failed to remove the failed job's staged NZB")
	}
	if entry.IsNZB() && m.usenet != nil {
		if err := m.usenet.Fail(entry.InfoHash, errors.New(g.State.Failure.Reason)); err != nil {
			m.logger.Warn().Err(err).Str("name", entry.Name).Msg("Failed to release the failed NZB")
		}
	}
	m.refreshArr(entry)
}

func (m *Manager) arrOwnsFailure(g grab.Grab) bool {
	if g.Entry == nil || g.Entry.Manual || g.State.PackKey != "" {
		return false
	}
	_, ok := m.Arr().Get(g.Entry.Category)
	return ok
}

func (m *Manager) failArr(g grab.Grab) string {
	if g.Entry == nil || !g.Entry.IsTorrent() || g.Entry.Manual || g.State.PackKey != "" {
		return ""
	}
	a, ok := m.Arr().Get(g.Entry.Category)
	if !ok {
		return ""
	}
	return a.Name
}
