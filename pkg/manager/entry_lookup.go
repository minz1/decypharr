package manager

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
)

func (m *Manager) lookupChild(ctx context.Context, call entryChildCall) entryMsg {
	hash := call.grab.State.Key
	if call.grab.Entry != nil {
		hash = cmp.Or(call.grab.Entry.InfoHash, hash)
	}
	clients := m.lookupClients(call.grab.State.SubmitAttempt.Provider)
	if len(clients) == 0 {
		return entryMsg{}
	}
	countFrom := call.settleFrom.Add(grab.LookupSettle)
	var firstAbsent time.Time
	for {
		at := time.Now()
		result, t := lookupAll(ctx, clients, hash)
		switch {
		case stopped(ctx) || result == debridTypes.LookupUnknown:
			return entryMsg{}
		case result == debridTypes.LookupFound:
			return m.lookupFound(call, t)
		case at.Before(countFrom):
		case firstAbsent.IsZero():
			firstAbsent = at
		case at.Sub(firstAbsent) >= grab.LookupSettle:
			return eventMsg(grab.Event{Kind: grab.EvLookup, Outcome: grab.OutcomeAbsent})
		}
		next := countFrom
		if !firstAbsent.IsZero() {
			next = firstAbsent.Add(grab.LookupSettle)
		}
		if sleepCtx(ctx, time.Until(next)) != nil {
			return entryMsg{}
		}
	}
}

func lookupAll(
	ctx context.Context,
	clients []debrid.Client,
	hash string,
) (debridTypes.LookupResult, *debridTypes.Torrent) {
	for _, c := range clients {
		result, t := c.Lookup(ctx, hash)
		switch {
		case result == debridTypes.LookupFound && t != nil:
			return result, t
		case result != debridTypes.LookupAbsent:
			return debridTypes.LookupUnknown, nil
		}
	}
	return debridTypes.LookupAbsent, nil
}

func (m *Manager) lookupClients(first string) []debrid.Client {
	clients := m.FilterDebrid(func(debrid.Client) bool { return true })
	i := slices.IndexFunc(clients, func(c debrid.Client) bool { return strings.EqualFold(c.Config().Name, first) })
	if i > 0 {
		head := clients[i]
		copy(clients[1:i+1], clients[:i])
		clients[0] = head
	}
	return clients
}

func (m *Manager) lookupFound(call entryChildCall, t *debridTypes.Torrent) entryMsg {
	var name, original string
	if call.grab.Entry != nil {
		name, original = call.grab.Entry.Name, call.grab.Entry.OriginalFilename
	}
	m.logger.Info().
		Str("name", name).
		Str("debrid", t.Debrid).
		Str("torrent_id", t.ID).
		Msg("Submit outcome found at the provider")
	found := &debridTypes.Torrent{
		ID:       t.ID,
		Debrid:   t.Debrid,
		InfoHash: t.InfoHash,
		Size:     t.GetSize(),
		Status:   debridTypes.TorrentStatusDownloading,
		Name:     cmp.Or(t.Name, name),
	}
	found.OriginalFilename = cmp.Or(t.OriginalFilename, original, found.Name)
	msg := eventMsg(grab.Event{Kind: grab.EvLookup, Outcome: grab.OutcomeFound, Provider: t.Debrid, TorrentID: t.ID})
	msg.torrent = found
	return msg
}
