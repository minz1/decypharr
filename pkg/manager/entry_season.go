package manager

import (
	"maps"
	"strings"
	"sync"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// packIndex maps season child keys to their packs. It starts as what the stored records imply and learns the
// seasons of packs added since.
type packIndex struct {
	mu    sync.Mutex
	packs map[string]string
}

func newPackIndex(packs map[string]string) *packIndex {
	return &packIndex{packs: packs}
}

func (p *packIndex) snapshot() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]string, len(p.packs))
	maps.Copy(out, p.packs)
	return out
}

func (p *packIndex) add(pack, season string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.packs == nil {
		p.packs = make(map[string]string)
	}
	p.packs[strings.ToLower(season)] = strings.ToLower(pack)
}

func terminalPhase(p grab.Phase) bool {
	return p == grab.PhaseComplete || p == grab.PhaseFailed
}

// seasonKeys are the keys of the season children of a pack, in season order; none for an entry that is no pack.
func (a *entryActor) seasonKeys(pack *storage.Entry) []string {
	seasons := a.seasonsOf(pack)
	keys := make([]string, 0, len(seasons))
	for _, s := range seasons {
		keys = append(keys, strings.ToLower(s.InfoHash))
	}
	return keys
}

// seasonRecord reads a season's queue record and its grab; ok is false for a season with no record.
func (a *entryActor) seasonRecord(key string) (grab.Grab, bool) {
	e, err := a.store.GetQueuedGrab(key)
	if err != nil {
		return grab.Grab{}, false
	}
	g, _ := grab.Load(e, a.packOf(), false)
	return g, true
}

// seasonsTerminal reports whether every season's record is terminal, and lists the seasons with no record.
func (a *entryActor) seasonsTerminal(seasons []*storage.Entry) (bool, []*storage.Entry) {
	terminal := true
	var missing []*storage.Entry
	for _, season := range seasons {
		g, ok := a.seasonRecord(strings.ToLower(season.InfoHash))
		switch {
		case !ok:
			terminal = false
			missing = append(missing, season)
		case !terminalPhase(g.State.Phase):
			terminal = false
		}
	}
	return terminal, missing
}

// packWaits completes a pack in its action once every season is terminal by the seasons' own records, and
// otherwise checks again after a delay: a ChildDone only wakes it. A check after the fan-out hands each season
// with no record to its actor again, so a season whose submit was refused or whose record went away is not
// waited for forever.
func (a *entryActor) packWaits(key string, st *entryState, ev grab.Event, fx *keyed.Effects[string, entryMsg]) {
	g := st.g
	if g.State.Phase != grab.PhaseActing || g.State.PackKey != "" || g.Entry == nil {
		return
	}
	seasons := a.seasonsOf(g.Entry)
	if len(seasons) == 0 {
		return
	}
	terminal, missing := a.seasonsTerminal(seasons)
	if terminal {
		a.step(key, st, eventMsg(grab.Event{Kind: grab.EvActionDone}), fx)
		return
	}
	if ev.Kind == grab.EvChildDone {
		a.resubmit(key, missing)
	}
	if st.hasSeasonTimer {
		fx.CancelTimer(st.seasonTimer)
	}
	st.seasonTimer = fx.After(a.retryDelay(0), eventMsg(grab.Event{Kind: grab.EvChildDone}))
	st.hasSeasonTimer = true
}

// resubmit hands each season to its actor again as a child of pack.
func (a *entryActor) resubmit(pack string, seasons []*storage.Entry) {
	for _, season := range seasons {
		msg := eventMsg(grab.Event{Kind: grab.EvSubmit, PackKey: pack})
		msg.entry = season.Clone()
		if err := a.send(strings.ToLower(season.InfoHash), msg); err != nil {
			a.logger.Debug().Err(err).Str("pack", pack).Str("season", season.Name).
				Msg("Missing season not handed on; the pack tries again on its next check")
		}
	}
}

// seasonEnded tells a season's pack that the season is terminal.
func (a *entryActor) seasonEnded(key string, cur, next grab.State) {
	if next.PackKey == "" || !terminalPhase(next.Phase) || terminalPhase(cur.Phase) {
		return
	}
	if err := a.send(next.PackKey, eventMsg(grab.Event{Kind: grab.EvChildDone})); err != nil {
		a.logger.Debug().Err(err).Str("key", key).Str("pack", next.PackKey).
			Msg("Season end not delivered; the pack finds it on its next check")
	}
}

// packEnded wakes the seasons of a pack that ended or was removed: a failed season is held while its pack runs, and
// nothing else tells it the pack is over.
func (a *entryActor) packEnded(key string, cur, next grab.Grab) {
	if cur.State.PackKey != "" || cur.Entry == nil || terminalPhase(cur.State.Phase) {
		return
	}
	if !terminalPhase(next.State.Phase) && next.State.Phase != grab.PhaseAbsent {
		return
	}
	for _, season := range a.seasonKeys(cur.Entry) {
		if err := a.send(season, entryMsg{deadline: true}); err != nil {
			a.logger.Warn().Err(err).Str("pack", key).Str("season", season).
				Msg("Pack end not delivered to a season; its failure is kept until a restart")
		}
	}
}

// deleteSeasons deletes the seasons of a deleted pack that are not terminal. A season's files go with it unless
// it shares the pack's folder.
func (a *entryActor) deleteSeasons(pack *storage.Entry, onlyLive bool) {
	naming := a.naming()
	for _, key := range a.seasonKeys(pack) {
		e, err := a.store.GetQueuedGrab(key)
		if err != nil {
			continue
		}
		g, _ := grab.Load(e, a.packOf(), false)
		if onlyLive && terminalPhase(g.State.Phase) {
			continue
		}
		ownFolder := e.DownloadPath(naming) != pack.DownloadPath(naming)
		msg := eventMsg(grab.Event{Kind: grab.EvDelete, Cleanup: grab.Cleanup{DeleteFiles: ownFolder}})
		if sendErr := a.send(key, msg); sendErr != nil {
			a.logger.Warn().Err(sendErr).Str("key", key).Msg("Season of a deleted pack not deleted")
		}
	}
}

// afterStep does what a saved move means for the seasons of a pack, or for the pack of a season.
func (a *entryActor) afterStep(
	key string,
	st *entryState,
	cur grab.Grab,
	ev grab.Event,
	fx *keyed.Effects[string, entryMsg],
) {
	a.seasonEnded(key, cur.State, st.g.State)
	a.packEnded(key, cur, st.g)
	switch {
	case ev.Kind == grab.EvChildDone || ev.Kind == grab.EvSeasonsStarted:
		a.packWaits(key, st, ev, fx)
	case ev.Kind == grab.EvDelete && cur.State.Phase == grab.PhaseActing && cur.State.PackKey == "" && cur.Entry != nil:
		a.deleteSeasons(cur.Entry, true)
	case ev.Kind == grab.EvActionNone && cur.State.PackKey == "" && cur.Entry != nil:
		a.deleteSeasons(cur.Entry, false)
	}
}

// seasonEntries are the season records a pack fans out into.
func (m *Manager) seasonEntries(pack *storage.Entry) []*storage.Entry {
	seasons := m.downloader.seasonsOf(pack)
	if len(seasons) == 0 {
		return nil
	}
	return convertToMultiSeason(pack, seasons)
}
