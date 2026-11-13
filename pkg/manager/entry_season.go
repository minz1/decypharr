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
	for _, n := range seasons {
		keys = append(keys, strings.ToLower(grab.SeasonHash(pack.InfoHash, n)))
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

func (a *entryActor) seasonsTerminal(keys []string) bool {
	for _, key := range keys {
		if g, ok := a.seasonRecord(key); !ok || !terminalPhase(g.State.Phase) {
			return false
		}
	}
	return true
}

// packWaits completes a pack in its action once every season is terminal by the seasons' own records, and
// otherwise checks again after a delay: a ChildDone only wakes it.
func (a *entryActor) packWaits(key string, st *entryState, fx *keyed.Effects[string, entryMsg]) {
	g := st.g
	if g.State.Phase != grab.PhaseActing || g.State.PackKey != "" || g.Entry == nil {
		return
	}
	keys := a.seasonKeys(g.Entry)
	if len(keys) == 0 {
		return
	}
	if a.seasonsTerminal(keys) {
		a.step(key, st, eventMsg(grab.Event{Kind: grab.EvActionDone}), fx)
		return
	}
	if st.hasSeasonTimer {
		fx.CancelTimer(st.seasonTimer)
	}
	st.seasonTimer = fx.After(a.retryDelay(0), eventMsg(grab.Event{Kind: grab.EvChildDone}))
	st.hasSeasonTimer = true
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
	switch {
	case ev.Kind == grab.EvChildDone || ev.Kind == grab.EvSeasonsStarted:
		a.packWaits(key, st, fx)
	case ev.Kind == grab.EvDelete && cur.State.Phase == grab.PhaseActing && cur.State.PackKey == "" && cur.Entry != nil:
		a.deleteSeasons(cur.Entry, true)
	case ev.Kind == grab.EvActionNone && cur.State.PackKey == "" && cur.Entry != nil:
		a.deleteSeasons(cur.Entry, false)
	}
}

// seasonNumbers are the season numbers a pack fans out into.
func (m *Manager) seasonNumbers(pack *storage.Entry) []int {
	seasons := m.downloader.seasonsOf(pack)
	numbers := make([]int, 0, len(seasons))
	for _, s := range seasons {
		numbers = append(numbers, s.SeasonNumber)
	}
	return numbers
}
